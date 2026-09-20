package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the object-storage session store (plan Step 22).
type S3Config struct {
	// Endpoint is the S3 endpoint ("s3.amazonaws.com", "localhost:9000" for MinIO).
	Endpoint string
	Bucket   string
	// Prefix is prepended to every key (default "sessions/").
	Prefix string
	Region string
	// AccessKey / SecretKey; empty falls back to the environment and the
	// instance/IRSA credential chain.
	AccessKey, SecretKey, SessionToken string
	// UseSSL defaults to true; set false for a local MinIO over http.
	UseSSL *bool
	// Timeout bounds one upload, download or copy (default 2 minutes).
	Timeout time.Duration
}

func (c S3Config) ssl() bool { return c.UseSSL == nil || *c.UseSSL }

func (c S3Config) prefix() string {
	p := c.Prefix
	if p == "" {
		p = "sessions/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return strings.TrimPrefix(p, "/")
}

func (c S3Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Minute
}

// S3 stores transcript snapshots as objects instead of files, which is what
// lets a worker run as a Kubernetes Job on any node: the next attempt restores
// the session from the bucket rather than from a volume that stayed behind on
// the node the last attempt happened to land on (design §4.4, §10 — "no PVC").
type S3 struct {
	Config S3Config

	mu     sync.Mutex
	client *minio.Client
}

// NewS3 validates the config. It does not contact the endpoint; the first
// Restore or Snapshot does.
func NewS3(cfg S3Config) (*S3, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("session s3: endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("session s3: bucket is required")
	}
	return &S3{Config: cfg}, nil
}

// Client lazily builds the MinIO/S3 client.
func (s *S3) Client() (*minio.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	var creds *credentials.Credentials
	if s.Config.AccessKey != "" {
		creds = credentials.NewStaticV4(s.Config.AccessKey, s.Config.SecretKey, s.Config.SessionToken)
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{}, &credentials.EnvMinio{}, &credentials.FileAWSCredentials{}, &credentials.IAM{},
		})
	}
	c, err := minio.New(s.Config.Endpoint, &minio.Options{Creds: creds, Secure: s.Config.ssl(), Region: s.Config.Region})
	if err != nil {
		return nil, fmt.Errorf("session s3 client: %w", err)
	}
	s.client = c
	return c, nil
}

// Key is the object key of one task's snapshot of a session.
func (s *S3) Key(taskID, sessionID string) (string, error) {
	if !taskIDRe.MatchString(taskID) {
		return "", fmt.Errorf("session: invalid task id %q", taskID)
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return "", fmt.Errorf("session: invalid session id %q", sessionID)
	}
	return s.Config.prefix() + taskID + "/" + sessionID + ".jsonl", nil
}

// URI is the s3:// address of a snapshot.
func (s *S3) URI(taskID, sessionID string) string {
	key, err := s.Key(taskID, sessionID)
	if err != nil {
		return ""
	}
	return "s3://" + s.Config.Bucket + "/" + key
}

// Restore downloads the snapshot into configDir/projects/<slug>/.
func (s *S3) Restore(ctx context.Context, taskID, sessionID, configDir, cwd string) error {
	key, err := s.Key(taskID, sessionID)
	if err != nil {
		return err
	}
	c, err := s.Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Config.timeout())
	defer cancel()
	obj, err := c.GetObject(ctx, s.Config.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return s3err(err, key)
	}
	defer func() { _ = obj.Close() }()
	// GetObject is lazy: touch it so a missing snapshot is ErrNotFound here
	// (the cold-retry signal) rather than a truncated file mid-copy.
	if _, err := obj.Stat(); err != nil {
		return s3err(err, key)
	}
	dstDir := filepath.Join(configDir, "projects", Slug(cwd))
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("session restore: %w", err)
	}
	return writeAtomic(filepath.Join(dstDir, sessionID+".jsonl"), obj)
}

// Snapshot uploads the transcript the CLI wrote under configDir.
func (s *S3) Snapshot(ctx context.Context, taskID, sessionID, configDir string) (string, error) {
	key, err := s.Key(taskID, sessionID)
	if err != nil {
		return "", err
	}
	src, err := TranscriptPath(configDir, sessionID)
	if err != nil {
		return "", err
	}
	c, err := s.Client()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Config.timeout())
	defer cancel()
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if _, err := c.PutObject(ctx, s.Config.Bucket, key, f, st.Size(), minio.PutObjectOptions{
		ContentType: "application/x-ndjson",
	}); err != nil {
		return "", fmt.Errorf("session s3 put %s: %w", key, err)
	}
	return "s3://" + s.Config.Bucket + "/" + key, nil
}

// Copy duplicates a snapshot into another task's prefix (fan-out fork, plan
// Step 21). The server copies the object; the bytes never travel through the
// harness.
func (s *S3) Copy(ctx context.Context, fromTaskID, toTaskID, sessionID string) (string, error) {
	from, err := s.Key(fromTaskID, sessionID)
	if err != nil {
		return "", err
	}
	to, err := s.Key(toTaskID, sessionID)
	if err != nil {
		return "", err
	}
	if from == to {
		return "s3://" + s.Config.Bucket + "/" + to, nil
	}
	c, err := s.Client()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Config.timeout())
	defer cancel()
	if _, err := c.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.Config.Bucket, Object: to},
		minio.CopySrcOptions{Bucket: s.Config.Bucket, Object: from}); err != nil {
		return "", s3err(err, from)
	}
	return "s3://" + s.Config.Bucket + "/" + to, nil
}

// Delete removes every snapshot under a task's prefix (retention sweep).
func (s *S3) Delete(ctx context.Context, taskID string) error {
	if !taskIDRe.MatchString(taskID) {
		return fmt.Errorf("session: invalid task id %q", taskID)
	}
	c, err := s.Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Config.timeout())
	defer cancel()
	prefix := s.Config.prefix() + taskID + "/"
	objects := c.ListObjects(ctx, s.Config.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	var errs []error
	for res := range c.RemoveObjects(ctx, s.Config.Bucket, objects, minio.RemoveObjectsOptions{}) {
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("session s3 delete %s: %w", res.ObjectName, res.Err))
		}
	}
	return errors.Join(errs...)
}

// s3err maps a missing key onto ErrNotFound, which the runner reads as "cold
// retry" rather than as an infrastructure failure.
func s3err(err error, key string) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return fmt.Errorf("session s3 %s: %w", key, err)
}

var _ Store = (*S3)(nil)

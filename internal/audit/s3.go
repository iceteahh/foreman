package audit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the object-storage sink (plan Step 17, design §7).
// Retention and WORM are bucket policies, not client settings: set
// object-lock/lifecycle on the bucket for retention.audit_ndjson_days.
type S3Config struct {
	// Endpoint is the S3 endpoint ("s3.amazonaws.com", "localhost:9000" for MinIO).
	Endpoint string
	Bucket   string
	// Prefix is prepended to every key (default "runs/").
	Prefix string
	Region string
	// AccessKey / SecretKey; empty falls back to the environment and the
	// instance/IRSA credential chain.
	AccessKey, SecretKey, SessionToken string
	// UseSSL defaults to true; set false for a local MinIO over http.
	UseSSL *bool
	// Spool is the local directory holding each run's log while it is being
	// written (default os.TempDir()/harness-audit). The file is uploaded on
	// Close and removed; a crash leaves it behind for `harness replay`.
	Spool string
	// PartSize is the multipart chunk in bytes (0 = library default).
	PartSize uint64
	// Timeout bounds one upload or download (default 5 minutes).
	Timeout time.Duration
}

// ssl reports the effective UseSSL.
func (c S3Config) ssl() bool { return c.UseSSL == nil || *c.UseSSL }

func (c S3Config) prefix() string {
	p := c.Prefix
	if p == "" {
		p = "runs/"
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
	return 5 * time.Minute
}

// S3Sink streams a run's NDJSON to a local spool file and uploads it on Close.
//
// Streaming straight to S3 would need a multipart writer held open for the
// whole run (up to 15 minutes) and would lose everything if the process died
// mid-run; a spool file keeps the local copy that `harness replay` can read
// even after a crash, and uploads exactly once.
type S3Sink struct {
	Config S3Config
	Logger *slog.Logger

	mu     sync.Mutex
	client *minio.Client
}

// NewS3Sink validates the config and prepares the spool directory. It does not
// contact the endpoint; the first Open does.
func NewS3Sink(cfg S3Config) (*S3Sink, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("audit s3: endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("audit s3: bucket is required")
	}
	s := &S3Sink{Config: cfg}
	if err := os.MkdirAll(s.spool(), 0o750); err != nil {
		return nil, fmt.Errorf("audit s3 spool: %w", err)
	}
	return s, nil
}

func (s *S3Sink) spool() string {
	if s.Config.Spool != "" {
		return s.Config.Spool
	}
	return filepath.Join(os.TempDir(), "harness-audit")
}

func (s *S3Sink) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Client lazily builds the MinIO/S3 client.
func (s *S3Sink) Client() (*minio.Client, error) {
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
			&credentials.EnvAWS{}, &credentials.EnvMinio{}, &credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}
	c, err := minio.New(s.Config.Endpoint, &minio.Options{Creds: creds, Secure: s.Config.ssl(), Region: s.Config.Region})
	if err != nil {
		return nil, fmt.Errorf("audit s3 client: %w", err)
	}
	s.client = c
	return c, nil
}

// Key is the object key for a run.
func (s *S3Sink) Key(runID string) string { return s.Config.prefix() + runID + ".ndjson" }

// URI is the s3:// address of a run's log.
func (s *S3Sink) URI(runID string) string {
	return "s3://" + s.Config.Bucket + "/" + s.Key(runID)
}

// Open starts a run's log.
func (s *S3Sink) Open(_ context.Context, runID string) (Writer, error) {
	if !runIDRe.MatchString(runID) {
		return nil, fmt.Errorf("audit: invalid run id %q", runID)
	}
	path := filepath.Join(s.spool(), runID+".ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit s3 spool open: %w", err)
	}
	return &s3Writer{sink: s, runID: runID, path: path,
		fileWriter: fileWriter{f: f, w: newBuf(f), uri: s.URI(runID)}}, nil
}

// Reader streams a run's log: the spool file when it is still there (a run in
// flight, or one whose upload failed), otherwise the object.
func (s *S3Sink) Reader(ctx context.Context, runID string) (io.ReadCloser, error) {
	if !runIDRe.MatchString(runID) {
		return nil, fmt.Errorf("audit: invalid run id %q", runID)
	}
	if f, err := os.Open(filepath.Join(s.spool(), runID+".ndjson")); err == nil {
		return f, nil
	}
	c, err := s.Client()
	if err != nil {
		return nil, err
	}
	obj, err := c.GetObject(ctx, s.Config.Bucket, s.Key(runID), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("audit s3 get %s: %w", s.Key(runID), err)
	}
	// GetObject is lazy: touch it so a missing key fails here, not mid-copy.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("audit s3 stat %s: %w", s.Key(runID), err)
	}
	return obj, nil
}

// Upload sends a spool file to the bucket. Exported so `harness replay` and an
// operator sweep can retry an upload that failed while the process was dying.
func (s *S3Sink) Upload(ctx context.Context, runID, path string) error {
	c, err := s.Client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.Config.timeout())
	defer cancel()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = c.PutObject(ctx, s.Config.Bucket, s.Key(runID), f, st.Size(), minio.PutObjectOptions{
		ContentType: "application/x-ndjson",
		PartSize:    s.Config.PartSize,
		UserMetadata: map[string]string{
			"harness-run-id": runID,
		},
	})
	if err != nil {
		return fmt.Errorf("audit s3 put %s: %w", s.Key(runID), err)
	}
	return nil
}

// s3Writer is a fileWriter that uploads on Close.
type s3Writer struct {
	fileWriter
	sink  *S3Sink
	runID string
	path  string
}

func (w *s3Writer) Close() error {
	if err := w.fileWriter.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.sink.Config.timeout())
	defer cancel()
	if err := w.sink.Upload(ctx, w.runID, w.path); err != nil {
		// Keep the spool file: the log is the audit trail, and Reader still
		// serves it. An operator (or a later run of the same id) can retry.
		w.sink.log().Error("audit upload failed; the spool file was kept", "run_id", w.runID, "path", w.path, "err", err)
		return err
	}
	if err := os.Remove(w.path); err != nil {
		w.sink.log().Warn("removing the audit spool file failed", "path", w.path, "err", err)
	}
	return nil
}

// ParseS3URL turns "s3://bucket/prefix/" or "http(s)://host/bucket/prefix/"
// into the bucket and prefix parts of an S3Config.
func ParseS3URL(raw string) (endpoint, bucket, prefix string, ssl bool, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", false, err
	}
	switch u.Scheme {
	case "s3":
		return "", u.Host, strings.TrimPrefix(u.Path, "/"), true, nil
	case "http", "https":
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
		if parts[0] == "" {
			return "", "", "", false, fmt.Errorf("audit s3: %q has no bucket", raw)
		}
		p := ""
		if len(parts) == 2 {
			p = parts[1]
		}
		return u.Host, parts[0], p, u.Scheme == "https", nil
	default:
		return "", "", "", false, fmt.Errorf("audit s3: unsupported scheme %q", u.Scheme)
	}
}

var _ Sink = (*S3Sink)(nil)

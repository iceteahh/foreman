//go:build docker

// S3 session snapshots against a real MinIO. No tokens; a Docker daemon is
// enough. Run them with `make docker-test`.
//
// worker.mode: k8s requires session.store: s3 (a Job may land on any node), so
// this is the store every retry in the scaled topology depends on — and until
// now the only one with no tests at all.
package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	"github.com/100xteam-ai/foreman/internal/session"
)

const (
	minioImage  = "quay.io/minio/minio:latest"
	minioUser   = "harness-test"
	minioSecret = "harness-test-secret" //nolint:gosec // throwaway container credential
	bucket      = "harness-sessions"
)

func docker(t *testing.T, timeout time.Duration, argv ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", argv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// minioContainer starts a throwaway MinIO and returns its endpoint. The port is
// assigned by the daemon so parallel runs (and a developer's own stack on 9000)
// do not collide.
func minioContainer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	name := "harness-session-minio-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if out, err := docker(t, 3*time.Minute, "run", "-d", "--rm", "--name", name,
		"-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER="+minioUser,
		"-e", "MINIO_ROOT_PASSWORD="+minioSecret,
		minioImage, "server", "/data"); err != nil {
		t.Skipf("cannot start MinIO: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = docker(t, time.Minute, "rm", "-f", name) })

	out, err := docker(t, 30*time.Second, "port", name, "9000/tcp")
	if err != nil {
		t.Fatalf("docker port: %v\n%s", err, out)
	}
	endpoint := strings.TrimSpace(strings.Split(out, "\n")[0])

	// Create the bucket with the same client the store uses. That waits for
	// the server to actually answer (not just for the container to exist) and
	// avoids the container's own `mc`, whose preconfigured alias does not carry
	// these credentials.
	client, err := newS3(t, endpoint).Client()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for {
		mkErr := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
		if mkErr == nil {
			break
		}
		if exists, existsErr := client.BucketExists(ctx, bucket); existsErr == nil && exists {
			break
		}
		if ctx.Err() != nil {
			logs, _ := docker(t, 20*time.Second, "logs", name)
			t.Fatalf("MinIO never became usable: %v\n%s", mkErr, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return endpoint
}

func newS3(t *testing.T, endpoint string) *session.S3 {
	t.Helper()
	ssl := false
	s, err := session.NewS3(session.S3Config{
		Endpoint: endpoint, Bucket: bucket, Prefix: "sessions/",
		AccessKey: minioUser, SecretKey: minioSecret, UseSSL: &ssl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeTranscript lays out a config dir the way the CLI does: one transcript
// under projects/<cwd-slug>/<session_id>.jsonl.
func writeTranscript(t *testing.T, configDir, cwd, sessionID, body string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", session.Slug(cwd))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func taskID(n int) string { return fmt.Sprintf("tsk_01J80000000000000000%04dX", n) }

// The round trip the whole retry story rests on: a worker writes a transcript,
// the harness snapshots it, and a *different* worker with an empty config dir
// and a different cwd restores it so `--resume` finds it.
func TestS3SnapshotRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newS3(t, minioContainer(t))
	root := t.TempDir()
	tk, sid := taskID(1), uuid.NewString()
	const body = `{"type":"user","text":"first attempt"}` + "\n"

	cfg1 := filepath.Join(root, "cfg1")
	writeTranscript(t, cfg1, filepath.Join(root, "ws", "run_1"), sid, body)
	uri, err := s.Snapshot(ctx, tk, sid, cfg1)
	if err != nil {
		t.Fatal(err)
	}
	if want := s.URI(tk, sid); uri != want {
		t.Errorf("uri %q, want %q", uri, want)
	}
	if !strings.HasPrefix(uri, "s3://"+bucket+"/sessions/"+tk+"/") {
		t.Errorf("uri is not in the task's namespace: %q", uri)
	}

	// A different node: empty config dir, different cwd, so a different slug.
	cfg2 := filepath.Join(root, "cfg2")
	cwd2 := filepath.Join(root, "ws", "run_2")
	if err := s.Restore(ctx, tk, sid, cfg2, cwd2); err != nil {
		t.Fatal(err)
	}
	p, err := session.TranscriptPath(cfg2, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(p)); got != session.Slug(cwd2) {
		t.Errorf("restored under slug %q, want %q", got, session.Slug(cwd2))
	}
	got, err := os.ReadFile(p) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("restored transcript %q, want %q", got, body)
	}
}

// A session nobody snapshotted must be ErrNotFound, not a generic S3 error:
// the pool tells "no transcript, cold retry" from "the store is broken" by
// exactly this, and getting it wrong turns a recoverable retry into a failure.
func TestS3RestoreMissingIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newS3(t, minioContainer(t))
	err := s.Restore(ctx, taskID(2), uuid.NewString(), t.TempDir(), t.TempDir())
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing snapshot: %v, want ErrNotFound", err)
	}
	if _, err := s.Snapshot(ctx, taskID(2), uuid.NewString(), t.TempDir()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("snapshot with no transcript: %v, want ErrNotFound", err)
	}
}

// A fan-out child forks the planner's session, and the runner restores by the
// task id it is running under — so the planner's snapshot has to exist in the
// child's namespace before the child starts.
func TestS3CopyPutsTheSnapshotInTheChildNamespace(t *testing.T) {
	ctx := context.Background()
	s := newS3(t, minioContainer(t))
	root := t.TempDir()
	parent, child, sid := taskID(3), taskID(4), uuid.NewString()
	const body = `{"type":"assistant","text":"the plan"}` + "\n"

	cfg := filepath.Join(root, "planner")
	writeTranscript(t, cfg, filepath.Join(root, "ws", "planner"), sid, body)
	if _, err := s.Snapshot(ctx, parent, sid, cfg); err != nil {
		t.Fatal(err)
	}

	uri, err := s.Copy(ctx, parent, child, sid)
	if err != nil {
		t.Fatal(err)
	}
	if want := s.URI(child, sid); uri != want {
		t.Errorf("copy uri %q, want the child's %q", uri, want)
	}

	// The child restores it under its own task id, keeping the session id.
	childCfg := filepath.Join(root, "child")
	if err := s.Restore(ctx, child, sid, childCfg, filepath.Join(root, "ws", "child")); err != nil {
		t.Fatalf("the child cannot read the forked transcript: %v", err)
	}
	p, _ := session.TranscriptPath(childCfg, sid)
	got, err := os.ReadFile(p) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("forked transcript %q, want %q", got, body)
	}
	// The parent keeps its own copy: a fork must not move the planner's.
	if err := s.Restore(ctx, parent, sid, filepath.Join(root, "parent-again"), root); err != nil {
		t.Errorf("the copy moved the parent's snapshot instead of duplicating it: %v", err)
	}
	// Copying a snapshot that does not exist is ErrNotFound.
	if _, err := s.Copy(ctx, parent, child, uuid.NewString()); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("copy of a missing snapshot: %v, want ErrNotFound", err)
	}
}

// Delete is the retention sweep (retention.sessions_days): it must remove every
// snapshot of a task and leave other tasks alone.
func TestS3DeleteRemovesOnlyThatTask(t *testing.T) {
	ctx := context.Background()
	s := newS3(t, minioContainer(t))
	root := t.TempDir()
	keep, drop := taskID(5), taskID(6)
	var dropped []string
	for i := 0; i < 3; i++ {
		sid := uuid.NewString()
		dropped = append(dropped, sid)
		cfg := filepath.Join(root, fmt.Sprintf("drop%d", i))
		writeTranscript(t, cfg, filepath.Join(root, "ws", fmt.Sprintf("d%d", i)), sid, "{}\n")
		if _, err := s.Snapshot(ctx, drop, sid, cfg); err != nil {
			t.Fatal(err)
		}
	}
	keepSID := uuid.NewString()
	keepCfg := filepath.Join(root, "keep")
	writeTranscript(t, keepCfg, filepath.Join(root, "ws", "k"), keepSID, "{}\n")
	if _, err := s.Snapshot(ctx, keep, keepSID, keepCfg); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, drop); err != nil {
		t.Fatal(err)
	}
	for _, sid := range dropped {
		if err := s.Restore(ctx, drop, sid, t.TempDir(), root); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("snapshot %s survived the sweep: %v", sid, err)
		}
	}
	if err := s.Restore(ctx, keep, keepSID, filepath.Join(root, "keep-restore"), root); err != nil {
		t.Errorf("the sweep took another task's snapshot: %v", err)
	}
	// Deleting a task with nothing stored is not an error (the sweeper retries).
	if err := s.Delete(ctx, taskID(7)); err != nil {
		t.Errorf("deleting an empty namespace: %v", err)
	}
}

// Ids reach the key builder from task records and the CLI, so a malformed one
// must be refused rather than allowed to shape an object key.
func TestS3KeyRejectsMalformedIDs(t *testing.T) {
	s := newS3(t, "127.0.0.1:1") // no server needed: Key never dials
	good := uuid.NewString()
	if _, err := s.Key(taskID(8), good); err != nil {
		t.Fatalf("a valid pair was rejected: %v", err)
	}
	for name, pair := range map[string][2]string{
		"path in task id":    {"../../etc/passwd", good},
		"empty task id":      {"", good},
		"session not a uuid": {taskID(8), "../../../etc/passwd"},
		"empty session":      {taskID(8), ""},
	} {
		if _, err := s.Key(pair[0], pair[1]); err == nil {
			t.Errorf("%s: accepted %q/%q", name, pair[0], pair[1])
		}
	}
}

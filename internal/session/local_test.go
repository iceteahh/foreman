package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := NewLocal(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	taskID := "tsk_01J8000000000000000000TEST"
	sid := uuid.NewString()

	// Worker 1 wrote a transcript under its cwd slug.
	cfg1 := filepath.Join(root, "cfg1")
	slugDir := filepath.Join(cfg1, "projects", "-ws-run-1")
	if err := os.MkdirAll(slugDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slugDir, sid+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	uri, err := l.Snapshot(ctx, taskID, sid, cfg1)
	if err != nil {
		t.Fatal(err)
	}
	if uri != "file://"+filepath.Join(root, "sessions", taskID, sid+".jsonl") {
		t.Errorf("uri %q", uri)
	}

	// Worker 2 has an empty config dir and a different cwd.
	cfg2 := filepath.Join(root, "cfg2")
	cwd2 := filepath.Join(root, "ws", "run_2")
	if err := l.Restore(ctx, taskID, sid, cfg2, cwd2); err != nil {
		t.Fatal(err)
	}
	p, err := TranscriptPath(cfg2, sid)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(p)) != Slug(cwd2) {
		t.Errorf("restored under %s, want slug %s", filepath.Dir(p), Slug(cwd2))
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{"type":"user"}`+"\n" {
		t.Errorf("content %q", b)
	}

	if err := l.Delete(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if err := l.Restore(ctx, taskID, sid, cfg2, cwd2); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}

func TestSnapshotMissingTranscript(t *testing.T) {
	l, _ := NewLocal(t.TempDir())
	_, err := l.Snapshot(context.Background(), "tsk_x", uuid.NewString(), t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestIDValidation(t *testing.T) {
	l, _ := NewLocal(t.TempDir())
	ctx := context.Background()
	if _, err := l.Snapshot(ctx, "../x", uuid.NewString(), t.TempDir()); err == nil {
		t.Error("bad task id accepted")
	}
	if err := l.Restore(ctx, "tsk_x", "../../etc/passwd", t.TempDir(), "."); err == nil {
		t.Error("bad session id accepted")
	}
	if err := l.Delete(ctx, "nope"); err == nil {
		t.Error("bad task id accepted on delete")
	}
}

func TestSlug(t *testing.T) {
	if got := Slug("/private/tmp/a.b/ws1"); got != "-private-tmp-a-b-ws1" {
		t.Errorf("slug %q", got)
	}
}

// A fan-out child is its own task, and the runner restores by the task id it is
// running under — so the planner's snapshot has to be copied into the child's
// namespace before the child starts, without moving the parent's.
func TestLocalCopyIntoChildNamespace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := NewLocal(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	parent := "tsk_01J8000000000000000PARENT"
	child := "tsk_01J80000000000000000CHILD"
	sid := uuid.NewString()
	const body = `{"type":"assistant","text":"the plan"}` + "\n"

	cfg := filepath.Join(root, "planner")
	dir := filepath.Join(cfg, "projects", Slug(filepath.Join(root, "ws", "planner")))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Snapshot(ctx, parent, sid, cfg); err != nil {
		t.Fatal(err)
	}

	uri, err := l.Copy(ctx, parent, child, sid)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file://" + filepath.Join(root, "sessions", child, sid+".jsonl"); uri != want {
		t.Errorf("copy uri %q, want %q", uri, want)
	}
	// The child can restore it, and the parent still has its own.
	childCfg := filepath.Join(root, "child-cfg")
	if err := l.Restore(ctx, child, sid, childCfg, filepath.Join(root, "ws", "child")); err != nil {
		t.Fatalf("the child cannot read the forked transcript: %v", err)
	}
	p, err := TranscriptPath(childCfg, sid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("forked transcript %q, want %q", got, body)
	}
	if err := l.Restore(ctx, parent, sid, filepath.Join(root, "parent-again"), root); err != nil {
		t.Errorf("the copy moved the parent's snapshot instead of duplicating it: %v", err)
	}

	// A snapshot that does not exist is ErrNotFound, not a generic failure:
	// the pool tells "fork impossible" from "the store is broken" by it.
	if _, err := l.Copy(ctx, parent, child, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Errorf("copy of a missing snapshot: %v, want ErrNotFound", err)
	}
	// Malformed ids must never shape a path.
	if _, err := l.Copy(ctx, "../../etc", child, sid); err == nil {
		t.Error("a task id containing a path was accepted")
	}
}

// NewS3 validates eagerly so a misconfigured store fails at startup (doctor,
// buildApp) rather than on the first retry of a real run.
func TestNewS3RequiresEndpointAndBucket(t *testing.T) {
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Error("an S3 store without an endpoint was accepted")
	}
	if _, err := NewS3(S3Config{Endpoint: "localhost:9000"}); err == nil {
		t.Error("an S3 store without a bucket was accepted")
	}
	s, err := NewS3(S3Config{Endpoint: "localhost:9000", Bucket: "b"})
	if err != nil {
		t.Fatal(err)
	}
	// The prefix always ends in a separator, so keys never run together.
	for _, tc := range []struct{ prefix, want string }{
		{"", "sessions/"},
		{"s", "s/"},
		{"s/", "s/"},
	} {
		s.Config.Prefix = tc.prefix
		key, err := s.Key("tsk_01J8000000000000000000TEST", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(key, tc.want) {
			t.Errorf("prefix %q produced key %q, want it to start with %q", tc.prefix, key, tc.want)
		}
	}
	// URI is empty rather than malformed when the ids are not usable.
	if got := s.URI("../../etc", uuid.NewString()); got != "" {
		t.Errorf("URI for a bad task id = %q, want empty", got)
	}
}

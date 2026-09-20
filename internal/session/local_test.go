package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

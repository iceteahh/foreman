package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Local stores snapshots at <Root>/<task_id>/<session_id>.jsonl.
type Local struct {
	Root string
}

// NewLocal creates root if needed.
func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("session root: %w", err)
	}
	return &Local{Root: root}, nil
}

var taskIDRe = regexp.MustCompile(`^tsk_[A-Za-z0-9_-]{1,64}$`)

func (l *Local) snapshotPath(taskID, sessionID string) (string, error) {
	if !taskIDRe.MatchString(taskID) {
		return "", fmt.Errorf("session: invalid task id %q", taskID)
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return "", fmt.Errorf("session: invalid session id %q", sessionID)
	}
	return filepath.Join(l.Root, taskID, sessionID+".jsonl"), nil
}

// Slug mirrors how the CLI names projects/<slug>: the absolute cwd with every
// path separator (and dot) replaced by '-'. Only used to pick a tidy restore
// location; lookup does not depend on it.
func Slug(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	r := strings.NewReplacer("/", "-", "\\", "-", ".", "-", ":", "-")
	return r.Replace(abs)
}

// TranscriptPath finds <configDir>/projects/*/<sessionID>.jsonl.
func TranscriptPath(configDir, sessionID string) (string, error) {
	if _, err := uuid.Parse(sessionID); err != nil {
		return "", fmt.Errorf("session: invalid session id %q", sessionID)
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", ErrNotFound
	case 1:
		return matches[0], nil
	default:
		// Pick the largest; the CLI only ever writes one, so this is defensive.
		best, bestSize := matches[0], int64(-1)
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && st.Size() > bestSize {
				best, bestSize = m, st.Size()
			}
		}
		return best, nil
	}
}

// Restore copies the snapshot into configDir/projects/<slug>/.
func (l *Local) Restore(_ context.Context, taskID, sessionID, configDir, cwd string) error {
	src, err := l.snapshotPath(taskID, sessionID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	dstDir := filepath.Join(configDir, "projects", Slug(cwd))
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("session restore: %w", err)
	}
	return copyFile(src, filepath.Join(dstDir, sessionID+".jsonl"))
}

// Snapshot copies the transcript out of configDir.
func (l *Local) Snapshot(_ context.Context, taskID, sessionID, configDir string) (string, error) {
	dst, err := l.snapshotPath(taskID, sessionID)
	if err != nil {
		return "", err
	}
	src, err := TranscriptPath(configDir, sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return "file://" + dst, nil
}

// Copy duplicates a snapshot into another task's directory (fan-out fork).
func (l *Local) Copy(_ context.Context, fromTaskID, toTaskID, sessionID string) (string, error) {
	src, err := l.snapshotPath(fromTaskID, sessionID)
	if err != nil {
		return "", err
	}
	dst, err := l.snapshotPath(toTaskID, sessionID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	if src == dst {
		return "file://" + dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return "", err
	}
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	return "file://" + dst, nil
}

// Delete removes <Root>/<task_id>.
func (l *Local) Delete(_ context.Context, taskID string) error {
	if !taskIDRe.MatchString(taskID) {
		return fmt.Errorf("session: invalid task id %q", taskID)
	}
	return os.RemoveAll(filepath.Join(l.Root, taskID))
}

// copyFile writes atomically via a temp file in the destination directory.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeAtomic(dst, in)
}

// writeAtomic streams r into dst through a temp file in the same directory, so
// a reader (or a crash) never sees a half-written transcript.
func writeAtomic(dst string, in io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".snap-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dst)
}

var _ Store = (*Local)(nil)

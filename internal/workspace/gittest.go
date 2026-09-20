package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Git runs git in dir for tests and fails the test on error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// NewBareRepo creates a bare origin with one commit on branch main containing
// files, and returns its path. Used by workspace, checks, and deliver tests.
func NewBareRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "origin.git")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	Git(t, src, "init", "--quiet", "-b", "main")
	for name, body := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, src, "add", "-A")
	Git(t, src, "commit", "--quiet", "-m", "init")
	Git(t, root, "clone", "--quiet", "--bare", src, bare)
	return bare
}

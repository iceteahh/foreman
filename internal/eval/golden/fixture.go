package golden

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fixtures materialises a case's fixture repository into a throwaway bare
// origin. Every case gets its own origin, so a case that pushes a bot branch
// cannot affect the next one, and the suite leaves the checked-in fixture
// untouched.
type Fixtures struct {
	// Root is the directory the origins are created under.
	Root string
	// Git is the git executable (default "git").
	Git string
}

// Materialize prepares c's fixture and returns the path of a bare repository
// the harness can clone from and push to.
func (f *Fixtures) Materialize(ctx context.Context, c *Case) (string, error) {
	origin := filepath.Join(f.Root, c.ID+".git")
	if err := os.MkdirAll(f.Root, 0o750); err != nil {
		return "", err
	}
	if _, err := os.Stat(origin); err == nil {
		if err := os.RemoveAll(origin); err != nil {
			return "", err
		}
	}
	src := c.fixturePath()
	if c.Repo.Bundle != "" {
		// A bundle is already a git object stream: clone it bare.
		if _, err := f.git(ctx, f.Root, "clone", "--quiet", "--bare", src, origin); err != nil {
			return "", fmt.Errorf("case %s: clone bundle %s: %w", c.ID, src, err)
		}
		return origin, f.assertBranch(ctx, origin, c)
	}
	work, err := f.commitTree(ctx, c, src)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(work) }()
	if _, err := f.git(ctx, f.Root, "clone", "--quiet", "--bare", work, origin); err != nil {
		return "", fmt.Errorf("case %s: clone fixture: %w", c.ID, err)
	}
	return origin, f.assertBranch(ctx, origin, c)
}

// commitTree copies dir into a scratch working repository and commits it as
// one initial commit on the case's branch. The fixture on disk is never
// turned into a git repository itself: nested .git directories inside the
// suite would confuse both git and reviewers.
func (f *Fixtures) commitTree(ctx context.Context, c *Case, dir string) (string, error) {
	work, err := os.MkdirTemp(f.Root, "fixture-"+c.ID+"-")
	if err != nil {
		return "", err
	}
	if err := copyTree(dir, work); err != nil {
		_ = os.RemoveAll(work)
		return "", fmt.Errorf("case %s: copy fixture %s: %w", c.ID, dir, err)
	}
	branch := c.Repo.Branch()
	steps := [][]string{
		{"init", "--quiet", "-b", branch},
		{"config", "user.name", "golden-fixture"},
		{"config", "user.email", "golden@harness.invalid"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "fixture: " + c.ID},
	}
	for _, s := range steps {
		if _, err := f.git(ctx, work, s...); err != nil {
			_ = os.RemoveAll(work)
			return "", fmt.Errorf("case %s: git %s: %w", c.ID, strings.Join(s, " "), err)
		}
	}
	return work, nil
}

// assertBranch fails loudly when the fixture has no branch matching repo.ref:
// the clone would otherwise fail later with a confusing "remote branch not
// found" in the middle of a paid run.
func (f *Fixtures) assertBranch(ctx context.Context, origin string, c *Case) error {
	out, err := f.git(ctx, origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+c.Repo.Branch())
	if err != nil || strings.TrimSpace(out) == "" {
		return fmt.Errorf("case %s: fixture has no branch %q", c.ID, c.Repo.Branch())
	}
	return nil
}

func (f *Fixtures) git(ctx context.Context, dir string, args ...string) (string, error) {
	bin := f.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // argv, never a shell
	cmd.Dir = dir
	// A fixture repo must not inherit the operator's git identity, hooks or
	// credential helpers: the suite has to behave the same on every machine.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// skipDirs are directories a fixture must never carry into its repository.
// `.git` would bring unrelated history; the rest are build and cache droppings
// left behind by anyone who ran the fixture's own tests by hand, and they
// would then be part of every case's base commit.
var skipDirs = map[string]bool{
	".git": true, "__pycache__": true, "node_modules": true, ".pytest_cache": true, ".venv": true,
}

// copyTree copies a directory tree, skipping skipDirs.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o750)
		}
		if !info.Mode().IsRegular() {
			return nil // symlinks and devices have no place in a fixture
		}
		return copyFile(p, filepath.Join(dst, rel), info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // suite-local path
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm()) //nolint:gosec // suite-local path
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

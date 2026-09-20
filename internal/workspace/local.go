package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// TokenSource yields a repository credential for a task. Return "" for
// anonymous access (public repos, local paths).
type TokenSource interface {
	Token(ctx context.Context, t *task.Task) (string, error)
}

// StaticToken is a TokenSource that always returns the same token.
type StaticToken string

// Token implements TokenSource.
func (s StaticToken) Token(context.Context, *task.Task) (string, error) { return string(s), nil }

// Local clones with the host git into <Root>/<run_id>.
type Local struct {
	Root string
	// Git is the git executable (default "git").
	Git string
	// CloneDepth defaults to 50 (design: `git clone --depth 50`).
	CloneDepth int
	// RepoBase turns "org/name" into a clone URL; default https://github.com/.
	RepoBase string
	// Tokens supplies credentials; nil means anonymous.
	Tokens TokenSource
	// Env is the allowlist of host variables passed to git and to Exec.
	Env []string
	// Username sent with the token (GitHub accepts x-access-token).
	Username string
	// BotName / BotEmail identify commits.
	BotName, BotEmail string
}

// NewLocal returns a manager with defaults filled in.
func NewLocal(root string) (*Local, error) {
	// Provision clones with cwd set to Root and the destination joined from it,
	// so a relative Root would nest the checkout under itself twice.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root %s: %w", root, err)
	}
	root = abs
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	return &Local{
		Root: root, Git: "git", CloneDepth: 50, RepoBase: "https://github.com/",
		Env:      []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SSH_AUTH_SOCK", "GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS", "NODE_PATH", "npm_config_cache"},
		Username: "x-access-token", BotName: "harness-bot", BotEmail: "harness-bot@users.noreply.github.com",
	}, nil
}

// CloneURL resolves the task's repo to something git can clone.
func (l *Local) CloneURL(repo string) string {
	switch {
	case strings.Contains(repo, "://"), strings.HasPrefix(repo, "git@"), strings.HasPrefix(repo, "/"), strings.HasPrefix(repo, "."), strings.HasPrefix(repo, "file:"):
		return repo
	default:
		base := l.RepoBase
		if base == "" {
			base = "https://github.com/"
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		if !strings.HasSuffix(repo, ".git") {
			repo += ".git"
		}
		return base + repo
	}
}

// Provision clones the task's repo at ref and creates the bot branch.
func (l *Local) Provision(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error) {
	if t.Workspace.Type != "git" {
		return nil, fmt.Errorf("workspace type %q unsupported", t.Workspace.Type)
	}
	dir := filepath.Join(l.Root, r.ID)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("workspace %s already exists", dir)
	}
	token := ""
	if l.Tokens != nil {
		var err error
		if token, err = l.Tokens.Token(ctx, t); err != nil {
			return nil, fmt.Errorf("workspace credential: %w", err)
		}
	}
	ws := &localWorkspace{l: l, dir: dir, branch: BranchFor(t), token: token}
	if err := ws.setupAskpass(); err != nil {
		return nil, err
	}
	depth := l.CloneDepth
	if depth <= 0 {
		depth = 50
	}
	args := []string{"clone", "--quiet", "--depth", strconv.Itoa(depth), "--branch", t.Workspace.Ref, "--single-branch", l.CloneURL(t.Workspace.Repo), dir}
	if res, err := ws.git(ctx, l.Root, args...); err != nil {
		_ = ws.Destroy()
		return nil, fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	for _, cfg := range [][]string{
		{"config", "user.name", l.BotName},
		{"config", "user.email", l.BotEmail},
		{"config", "commit.gpgsign", "false"},
	} {
		if res, err := ws.git(ctx, dir, cfg...); err != nil {
			_ = ws.Destroy()
			return nil, fmt.Errorf("git %v: %w: %s", cfg, err, res.Stderr)
		}
	}
	res, err := ws.git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		_ = ws.Destroy()
		return nil, fmt.Errorf("git rev-parse: %w", err)
	}
	ws.base = strings.TrimSpace(res.Stdout)
	if res, err := ws.git(ctx, dir, "checkout", "--quiet", "-B", ws.branch); err != nil {
		_ = ws.Destroy()
		return nil, fmt.Errorf("git checkout -B %s: %w: %s", ws.branch, err, res.Stderr)
	}
	return ws, nil
}

// Open reattaches to <Root>/<run_id> (kept for review or after a failed
// delivery). The base commit is origin/<ref>, which the single-branch clone
// tracks; the bot branch must be checked out.
func (l *Local) Open(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error) {
	dir := filepath.Join(l.Root, r.ID)
	if r.Worker.WorkspacePath != "" {
		dir = r.Worker.WorkspacePath
	}
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNoWorkspace, dir)
	}
	token := ""
	if l.Tokens != nil {
		var err error
		if token, err = l.Tokens.Token(ctx, t); err != nil {
			return nil, fmt.Errorf("workspace credential: %w", err)
		}
	}
	ws := &localWorkspace{l: l, dir: dir, branch: BranchFor(t), token: token}
	if err := ws.setupAskpass(); err != nil {
		return nil, err
	}
	res, err := ws.git(ctx, dir, "rev-parse", "--verify", "refs/remotes/origin/"+t.Workspace.Ref)
	if err != nil {
		_ = os.RemoveAll(ws.askpassDir)
		return nil, fmt.Errorf("workspace open: base commit: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	ws.base = strings.TrimSpace(res.Stdout)
	res, err = ws.git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(res.Stdout) != ws.branch {
		_ = os.RemoveAll(ws.askpassDir)
		return nil, fmt.Errorf("workspace open: expected branch %s checked out, found %q", ws.branch, strings.TrimSpace(res.Stdout))
	}
	return ws, nil
}

type localWorkspace struct {
	l          *Local
	dir        string
	branch     string
	base       string
	token      string
	askpassDir string
}

func (w *localWorkspace) Path() string    { return w.dir }
func (w *localWorkspace) Branch() string  { return w.branch }
func (w *localWorkspace) BaseSHA() string { return w.base }

// setupAskpass writes a GIT_ASKPASS helper outside the workspace. The helper
// reads the token from an environment variable that only git receives, so the
// secret is never on disk and never in .git/config.
func (w *localWorkspace) setupAskpass() error {
	if w.token == "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "harness-askpass-")
	if err != nil {
		return err
	}
	script := "#!/bin/sh\ncase \"$1\" in\n  *sername*) printf '%s' \"$HARNESS_GIT_USERNAME\" ;;\n  *) printf '%s' \"$HARNESS_GIT_TOKEN\" ;;\nesac\n"
	p := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil { //nolint:gosec // must be executable
		_ = os.RemoveAll(dir)
		return err
	}
	w.askpassDir = dir
	return nil
}

// baseEnv is the allowlisted host environment.
func (w *localWorkspace) baseEnv() []string {
	env := make([]string, 0, len(w.l.Env)+4)
	for _, k := range w.l.Env {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// gitEnv adds credential plumbing for the harness's own git calls. The host's
// global and system git config are ignored: stored credential helpers
// (Keychain, gh) would otherwise answer with the operator's account before
// GIT_ASKPASS is consulted, and URL rewrites or hooks from ~/.gitconfig must
// not leak into worker checkouts.
func (w *localWorkspace) gitEnv() []string {
	env := append(w.baseEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if w.token != "" && w.askpassDir != "" {
		env = append(env,
			"GIT_ASKPASS="+filepath.Join(w.askpassDir, "askpass.sh"),
			"HARNESS_GIT_USERNAME="+w.l.Username,
			"HARNESS_GIT_TOKEN="+w.token,
		)
	}
	return env
}

func (w *localWorkspace) git(ctx context.Context, dir string, args ...string) (ExecResult, error) {
	git := w.l.Git
	if git == "" {
		git = "git"
	}
	// An empty credential.helper resets the helper list from every config level.
	full := append([]string{"-c", "credential.helper="}, args...)
	return runCmd(ctx, dir, w.gitEnv(), git, full...)
}

// Exec runs a command with the workspace allowlist env (no git credentials).
func (w *localWorkspace) Exec(ctx context.Context, name string, args ...string) (ExecResult, error) {
	return runCmd(ctx, w.dir, w.baseEnv(), name, args...)
}

func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) (ExecResult, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := ExecResult{Cmd: append([]string{name}, args...), Stdout: out.String(), Stderr: errb.String(), Duration: time.Since(start)}
	if ctx.Err() != nil {
		res.TimedOut = true
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == -1 {
			res.ExitCode = 137
		}
	default:
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s exited %d", name, res.ExitCode)
	}
	return res, nil
}

// Capture stages all changes and diffs against the base commit.
func (w *localWorkspace) Capture(ctx context.Context) (*Capture, error) {
	if res, err := w.git(ctx, w.dir, "add", "-A"); err != nil {
		return nil, fmt.Errorf("git add: %w: %s", err, res.Stderr)
	}
	c := &Capture{BaseSHA: w.base, Branch: w.branch}
	res, err := w.git(ctx, w.dir, "diff", "--cached", "--no-color", w.base)
	if err != nil {
		return nil, fmt.Errorf("git diff: %w: %s", err, res.Stderr)
	}
	c.Diff = res.Stdout
	res, err = w.git(ctx, w.dir, "diff", "--cached", "--name-only", w.base)
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	c.ChangedFiles = splitLines(res.Stdout)
	res, err = w.git(ctx, w.dir, "diff", "--cached", "--numstat", w.base)
	if err != nil {
		return nil, fmt.Errorf("git diff --numstat: %w", err)
	}
	stats := map[string]*FileStat{}
	for _, line := range splitLines(res.Stdout) {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fs := &FileStat{Path: parts[2]}
		if parts[0] == "-" {
			fs.Binary = true
		} else {
			fs.Added, _ = strconv.Atoi(parts[0])
			fs.Deleted, _ = strconv.Atoi(parts[1])
		}
		// renames appear as "old => new" or "{a => b}/x"
		if i := strings.Index(fs.Path, " => "); i >= 0 && !strings.Contains(fs.Path, "{") {
			fs.OldPath, fs.Path = fs.Path[:i], fs.Path[i+4:]
		}
		stats[fs.Path] = fs
		c.Stats = append(c.Stats, *fs)
	}
	res, err = w.git(ctx, w.dir, "diff", "--cached", "--name-status", w.base)
	if err != nil {
		return nil, fmt.Errorf("git diff --name-status: %w", err)
	}
	for _, line := range splitLines(res.Stdout) {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		path := parts[len(parts)-1]
		for i := range c.Stats {
			if c.Stats[i].Path == path {
				c.Stats[i].Status = parts[0][:1]
				if len(parts) == 3 {
					c.Stats[i].OldPath = parts[1]
				}
			}
		}
	}
	res, err = w.git(ctx, w.dir, "rev-parse", "HEAD")
	if err == nil {
		if head := strings.TrimSpace(res.Stdout); head != w.base {
			c.HeadSHA = head
		}
	}
	return c, nil
}

// Commit commits the staged tree. Returns "" when there is nothing to commit.
func (w *localWorkspace) Commit(ctx context.Context, message string) (string, error) {
	if res, err := w.git(ctx, w.dir, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w: %s", err, res.Stderr)
	}
	if _, err := w.git(ctx, w.dir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil // clean index
	}
	if res, err := w.git(ctx, w.dir, "commit", "--quiet", "--no-verify", "-m", message); err != nil {
		return "", fmt.Errorf("git commit: %w: %s", err, res.Stderr)
	}
	res, err := w.git(ctx, w.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Push publishes the bot branch. The clone is single-branch, so no
// remote-tracking ref exists for the bot branch; the lease is therefore taken
// explicitly against what origin currently has (or against "absent").
func (w *localWorkspace) Push(ctx context.Context) error {
	res, err := w.git(ctx, w.dir, "ls-remote", "--heads", "origin", w.branch)
	if err != nil {
		return fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	expected := ""
	if f := strings.Fields(res.Stdout); len(f) >= 1 {
		expected = f[0]
	}
	lease := "--force-with-lease=refs/heads/" + w.branch + ":" + expected
	if res, err := w.git(ctx, w.dir, "push", "--quiet", lease, "--set-upstream", "origin", w.branch+":refs/heads/"+w.branch); err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Destroy removes the checkout and the askpass helper.
func (w *localWorkspace) Destroy() error {
	var errs []error
	if w.askpassDir != "" {
		errs = append(errs, os.RemoveAll(w.askpassDir))
	}
	errs = append(errs, os.RemoveAll(w.dir))
	return errors.Join(errs...)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var (
	_ Workspace = (*localWorkspace)(nil)
	_ Manager   = (*Local)(nil)
)

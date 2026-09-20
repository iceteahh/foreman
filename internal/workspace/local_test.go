package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

func testTask(repo string) *task.Task {
	return &task.Task{ID: "tsk_01J8000000000000000000TEST", Kind: task.KindCodeFix, Prompt: "p",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: repo, Ref: "main"},
		Policy:    task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 1, TimeoutMS: 1000, MaxCostUSD: 1}}
}

func TestProvisionCaptureCommitPush(t *testing.T) {
	ctx := context.Background()
	origin := NewBareRepo(t, map[string]string{"README.md": "hi\n", "src/a.go": "package a\n", "src/a_test.go": "package a\n// t1\n// t2\n"})
	mgr, err := NewLocal(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.Tokens = StaticToken("secret-token") // exercised even though the local origin ignores it
	tk := testTask(origin)
	r := task.NewRun(tk, time.Now())
	ws, err := mgr.Provision(ctx, tk, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Destroy() })

	if ws.Branch() != "harness/"+tk.ID || ws.BaseSHA() == "" || filepath.Base(ws.Path()) != r.ID {
		t.Errorf("ws: branch=%s base=%s path=%s", ws.Branch(), ws.BaseSHA(), ws.Path())
	}
	if out := Git(t, ws.Path(), "branch", "--show-current"); strings.TrimSpace(out) != ws.Branch() {
		t.Errorf("current branch %q", out)
	}
	// The token must not land anywhere in the checkout.
	cfg, _ := os.ReadFile(filepath.Join(ws.Path(), ".git", "config"))
	if strings.Contains(string(cfg), "secret-token") {
		t.Error("token written to .git/config")
	}

	// Exec uses the allowlist env, not the harness env.
	t.Setenv("ANTHROPIC_API_KEY", "leak")
	res, err := ws.Exec(ctx, "sh", "-c", "echo ${ANTHROPIC_API_KEY:-unset}; pwd")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, "unset\n") || !strings.Contains(res.Stdout, r.ID) {
		t.Errorf("exec env/cwd: %q", res.Stdout)
	}
	if _, err := ws.Exec(ctx, "sh", "-c", "exit 3"); err == nil {
		t.Error("non-zero exit should error")
	} else if res, _ := ws.Exec(ctx, "sh", "-c", "exit 3"); res.ExitCode != 3 {
		t.Errorf("exit code %d", res.ExitCode)
	}

	// Empty capture.
	c, err := ws.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Diff != "" || len(c.ChangedFiles) != 0 {
		t.Errorf("expected empty capture, got %+v", c)
	}

	// Worker edits: modify, add (untracked), delete a test.
	if err := os.WriteFile(filepath.Join(ws.Path(), "src/a.go"), []byte("package a\n\nfunc A() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path(), "src/new.go"), []byte("package a\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws.Path(), "src/a_test.go")); err != nil {
		t.Fatal(err)
	}
	c, err = ws.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.ChangedFiles, ",") != "src/a.go,src/a_test.go,src/new.go" {
		t.Errorf("changed files %v", c.ChangedFiles)
	}
	if !strings.Contains(c.Diff, "+func A() {}") {
		t.Errorf("diff missing edit:\n%s", c.Diff)
	}
	byPath := map[string]FileStat{}
	for _, s := range c.Stats {
		byPath[s.Path] = s
	}
	if byPath["src/a_test.go"].Status != "D" || byPath["src/new.go"].Status != "A" || byPath["src/a.go"].Status != "M" || byPath["src/a.go"].Added != 2 {
		t.Errorf("stats %+v", byPath)
	}

	sha, err := ws.Commit(ctx, "harness: fix")
	if err != nil || sha == "" || sha == ws.BaseSHA() {
		t.Fatalf("commit sha=%q err=%v", sha, err)
	}
	if again, _ := ws.Commit(ctx, "noop"); again != "" {
		t.Error("second commit with clean tree should be a no-op")
	}
	if err := ws.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if out := Git(t, origin, "rev-parse", ws.Branch()); strings.TrimSpace(out) != sha {
		t.Errorf("origin branch at %s want %s", out, sha)
	}
	// Push again after an amend: force-with-lease lets retries update the branch.
	Git(t, ws.Path(), "commit", "--quiet", "--amend", "--no-edit", "-m", "harness: fix v2")
	if err := ws.Push(ctx); err != nil {
		t.Fatalf("re-push: %v", err)
	}

	path := ws.Path()
	if err := ws.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("workspace not removed")
	}
}

func TestProvisionErrors(t *testing.T) {
	ctx := context.Background()
	origin := NewBareRepo(t, map[string]string{"f": "x"})
	mgr, _ := NewLocal(filepath.Join(t.TempDir(), "ws"))
	tk := testTask(origin)
	tk.Workspace.Ref = "does-not-exist"
	if _, err := mgr.Provision(ctx, tk, task.NewRun(tk, time.Now())); err == nil {
		t.Error("bad ref accepted")
	}
	entries, _ := os.ReadDir(mgr.Root)
	if len(entries) != 0 {
		t.Error("failed provision left a directory behind")
	}
	tk.Workspace.Type = "svn"
	if _, err := mgr.Provision(ctx, tk, task.NewRun(tk, time.Now())); err == nil {
		t.Error("non-git type accepted")
	}
}

func TestCloneURL(t *testing.T) {
	mgr, _ := NewLocal(t.TempDir())
	for in, want := range map[string]string{
		"org/svc":                    "https://github.com/org/svc.git",
		"org/svc.git":                "https://github.com/org/svc.git",
		"https://gitlab.com/o/s.git": "https://gitlab.com/o/s.git",
		"git@github.com:o/s.git":     "git@github.com:o/s.git",
		"/tmp/x/origin.git":          "/tmp/x/origin.git",
	} {
		if got := mgr.CloneURL(in); got != want {
			t.Errorf("CloneURL(%q) = %q want %q", in, got, want)
		}
	}
}

func TestExecTimeout(t *testing.T) {
	origin := NewBareRepo(t, map[string]string{"f": "x"})
	mgr, _ := NewLocal(filepath.Join(t.TempDir(), "ws"))
	tk := testTask(origin)
	ws, err := mgr.Provision(context.Background(), tk, task.NewRun(tk, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Destroy()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := ws.Exec(ctx, "sleep", "5")
	if err == nil || !res.TimedOut {
		t.Errorf("expected timeout, got err=%v res=%+v", err, res)
	}
}

func TestOpenReattachesKeptWorkspace(t *testing.T) {
	origin := NewBareRepo(t, map[string]string{"a.txt": "a\n"})
	m, _ := NewLocal(t.TempDir())
	tk := &task.Task{ID: task.NewTaskID(), Kind: task.KindCodeFix, Workspace: task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"}}
	r := task.NewRun(tk, time.Now())
	ctx := context.Background()
	ws, err := m.Provision(ctx, tk, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path(), "b.txt"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Worker.WorkspacePath = ws.Path()
	// Reopen: same base, same branch, the uncommitted change is still there.
	re, err := m.Open(ctx, tk, r)
	if err != nil {
		t.Fatal(err)
	}
	if re.Path() != ws.Path() || re.BaseSHA() != ws.BaseSHA() || re.Branch() != ws.Branch() {
		t.Errorf("reopened %s %s %s", re.Path(), re.BaseSHA(), re.Branch())
	}
	c, err := re.Capture(ctx)
	if err != nil || len(c.ChangedFiles) != 1 || c.ChangedFiles[0] != "b.txt" {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := re.Commit(ctx, "kept"); err != nil {
		t.Fatal(err)
	}
	if err := re.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if out := Git(t, origin, "log", "--oneline", ws.Branch()); !strings.Contains(out, "kept") {
		t.Errorf("%s", out)
	}
	if err := re.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(ctx, tk, r); !errors.Is(err, ErrNoWorkspace) {
		t.Errorf("err %v", err)
	}
}

// Provision clones with cwd set to Root and the destination joined from it, so
// a relative Root used to nest the checkout under itself twice.
func TestNewLocalMakesRootAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	mgr, err := NewLocal("ws")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(mgr.Root) {
		t.Fatalf("Root %q is not absolute", mgr.Root)
	}
}

package deliver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

func setup(t *testing.T) (*Input, string) {
	t.Helper()
	origin := workspace.NewBareRepo(t, map[string]string{"README.md": "hi\n", "src/a.go": "package a\n"})
	tk := &task.Task{ID: "tsk_01J8000000000000000000TEST", Kind: task.KindCodeFix, Prompt: "# Fix the crash on empty input\nmore detail",
		Workspace:   task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"},
		Policy:      task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 1, TimeoutMS: 1000, MaxCostUSD: 1},
		RequestedBy: "webhook:github:issue-7"}
	r := task.NewRun(tk, time.Now())
	mgr, _ := workspace.NewLocal(filepath.Join(t.TempDir(), "ws"))
	ws, err := mgr.Provision(context.Background(), tk, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Destroy() })
	if err := os.WriteFile(filepath.Join(ws.Path(), "src/a.go"), []byte("package a\n\nfunc Fixed() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	rep := checks.Report{Results: []checks.Result{{Name: "exit_code", Status: checks.Pass}, {Name: "diff_scope", Status: checks.Pass}}}
	return &Input{Task: tk, Run: r, Workspace: ws, Checks: rep}, origin
}

func TestBranchPush(t *testing.T) {
	in, origin := setup(t)
	arts, err := (BranchPush{}).Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || !strings.HasPrefix(arts[0], "branch:harness/tsk_01J8000000000000000000TEST@") {
		t.Errorf("artifacts %v", arts)
	}
	sha := strings.SplitN(arts[0], "@", 2)[1]
	if got := strings.TrimSpace(workspace.Git(t, origin, "rev-parse", "harness/"+in.Task.ID)); got != sha {
		t.Errorf("origin at %s want %s", got, sha)
	}
	msg := workspace.Git(t, origin, "log", "-1", "--format=%B", sha)
	if !strings.Contains(msg, "harness: Fix the crash on empty input") || !strings.Contains(msg, in.Run.ID) {
		t.Errorf("commit message:\n%s", msg)
	}
	// Idempotent: a second delivery with no new changes re-pushes the same sha.
	arts2, err := (BranchPush{}).Deliver(context.Background(), in)
	if err != nil || arts2[0] != arts[0] {
		t.Errorf("second delivery %v %v", arts2, err)
	}
}

func TestBranchPushNothingToDeliver(t *testing.T) {
	in, _ := setup(t)
	_ = os.WriteFile(filepath.Join(in.Workspace.Path(), "src/a.go"), []byte("package a\n"), 0o640) // revert
	if _, err := (BranchPush{}).Deliver(context.Background(), in); err == nil || !strings.Contains(err.Error(), "nothing to deliver") {
		t.Errorf("err %v", err)
	}
}

func TestSubjectAndCloses(t *testing.T) {
	in, _ := setup(t)
	if got := Title(in); got != "[harness] Fix the crash on empty input" {
		t.Errorf("fallback title %q", got)
	}
	if closesLine(in) != "" {
		t.Error("non-webhook task must not close an issue")
	}
	in.Task.Title = "Greet should not produce 'Hello, ' for a blank name (#2)"
	in.Task.Workspace.Repo = "iceteahh/test-harness"
	in.Task.RequestedBy = "webhook:github:iceteahh/test-harness#2"
	if got := Title(in); got != "[harness] "+in.Task.Title {
		t.Errorf("title %q", got)
	}
	if !strings.HasPrefix(CommitMessage(in), "harness: Greet should not produce 'Hello, ' for a blank name (#2)\n") {
		t.Errorf("commit %q", CommitMessage(in))
	}
	if !strings.Contains(Body(in), "Closes #2\n") {
		t.Errorf("body lacks Closes:\n%s", Body(in))
	}
	in.Task.RequestedBy = "webhook:github:other/repo#2"
	if closesLine(in) != "" {
		t.Error("issue from another repo must not be closed")
	}
}

func TestOwnerRepo(t *testing.T) {
	for in, want := range map[string]string{
		"org/svc": "org/svc", "org/svc.git": "org/svc", "https://github.com/org/svc.git": "org/svc",
		"https://ghe.corp/org/svc": "org/svc", "git@github.com:org/svc.git": "org/svc",
	} {
		o, r, err := OwnerRepo(in)
		if err != nil || o+"/"+r != want {
			t.Errorf("OwnerRepo(%q) = %s/%s %v", in, o, r, err)
		}
	}
	for _, bad := range []string{"/tmp/x/origin.git", "svc", "a/b/c", "./x"} {
		if _, _, err := OwnerRepo(bad); err == nil {
			t.Errorf("OwnerRepo(%q) accepted", bad)
		}
	}
}

// fakeGitHub records PR API calls and returns canned responses.
type fakeGitHub struct {
	t       *testing.T
	open    []map[string]any
	created []map[string]any
	edited  []map[string]any
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/org/svc/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head") != "org:harness/tsk_01J8000000000000000000TEST" || r.URL.Query().Get("state") != "open" {
			f.t.Errorf("list query %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(f.open)
	})
	mux.HandleFunc("POST /repos/org/svc/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.created = append(f.created, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/org/svc/pull/42"})
	})
	mux.HandleFunc("PATCH /repos/org/svc/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.edited = append(f.edited, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/org/svc/pull/42"})
	})
	return mux
}

func TestGitHubPRUpsert(t *testing.T) {
	in, _ := setup(t)
	fake := &fakeGitHub{t: t}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	// The workspace's origin is the local bare repo; only the PR API is faked.
	// OwnerRepo needs a GitHub-shaped repo string, so point the task at org/svc
	// while the workspace already cloned the local origin.
	in.Task.Workspace.Repo = "org/svc"
	g, err := NewGitHubPR("tok", srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}

	arts, err := g.Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 || arts[0] != "pr:org/svc#42" || !strings.HasPrefix(arts[1], "branch:harness/") {
		t.Errorf("artifacts %v", arts)
	}
	if len(fake.created) != 1 || len(fake.edited) != 0 {
		t.Fatalf("created=%d edited=%d", len(fake.created), len(fake.edited))
	}
	c := fake.created[0]
	if c["head"] != "harness/tsk_01J8000000000000000000TEST" || c["base"] != "main" || c["draft"] != true ||
		c["title"] != "[harness] Fix the crash on empty input" || !strings.Contains(c["body"].(string), "| exit_code | ✅ pass |") ||
		!strings.Contains(c["body"].(string), "webhook:github:issue-7") {
		t.Errorf("create body %+v", c)
	}

	// Second delivery finds the open PR and updates it instead of creating another.
	fake.open = []map[string]any{{"number": 42, "html_url": "https://github.com/org/svc/pull/42"}}
	in.Run.Attempt = 2
	arts, err = g.Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if arts[0] != "pr:org/svc#42" || len(fake.created) != 1 || len(fake.edited) != 1 || !strings.Contains(fake.edited[0]["body"].(string), "attempt 2") {
		t.Errorf("upsert: arts=%v created=%d edited=%+v", arts, len(fake.created), fake.edited)
	}
}

func TestGitHubPRNonGitHubRepo(t *testing.T) {
	in, _ := setup(t)
	g, _ := NewGitHubPR("", "", true)
	if _, err := g.Deliver(context.Background(), in); err == nil || !strings.Contains(err.Error(), "owner/repo") {
		t.Errorf("err %v", err)
	}
}

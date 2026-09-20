//go:build live

package judge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/session"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// TestLiveJudgeGoodAndBadDiff spends a few cents: two haiku judge calls on a
// known-good and a known-bad change. Run with `make live`.
func TestLiveJudgeGoodAndBadDiff(t *testing.T) {
	key, oauth := os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if key == "" && oauth == "" {
		t.Skip("neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set")
	}
	root := t.TempDir()
	sess, _ := session.NewLocal(filepath.Join(root, "sessions"))
	aud, _ := audit.NewFileSink(filepath.Join(root, "audit"))
	rn := &runner.Runner{APIKey: key, OAuthToken: oauth, Sessions: sess, Audit: aud}
	j := &Judge{Spawner: rn, Options: Options{Model: "haiku", MaxTurns: 4, MaxCostUSD: 0.15, Timeout: 2 * time.Minute}}

	greet := "package demo\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n"
	origin := workspace.NewBareRepo(t, map[string]string{"go.mod": "module demo\n\ngo 1.22\n", "greet.go": greet})
	mgr, _ := workspace.NewLocal(filepath.Join(root, "ws"))
	tk := &task.Task{ID: task.NewTaskID(), Kind: task.KindCodeFix, Title: "farewell",
		Prompt:     "Add a function Farewell(name string) string returning \"Goodbye, <name>\" to greet.go, with a test in greet_test.go.",
		Workspace:  task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"},
		Policy:     task.Policy{AllowedTools: []string{"Read", "Edit"}, MaxTurns: 10, TimeoutMS: 60000, MaxCostUSD: 1, Judge: task.JudgePolicy{Enabled: true, Samples: 1, Threshold: 7}},
		Acceptance: task.Acceptance{Commands: []string{"go test ./..."}, DiffScope: []string{"*.go"}}}
	rep := checks.Report{Results: []checks.Result{{Name: "exit_code", Status: checks.Pass, Evidence: "ok"}, {Name: "acceptance_commands", Status: checks.Pass, Evidence: "$ go test ./...\nok"}}}

	judgeOf := func(files map[string]string) *Verdict {
		r := task.NewRun(tk, time.Now())
		ws, err := mgr.Provision(context.Background(), tk, r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ws.Destroy() }()
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(ws.Path(), name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cap, err := ws.Capture(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		v, err := j.Evaluate(context.Background(), &Input{Task: tk, Run: r, Checks: rep, Capture: cap, Workspace: ws.Path(), ConfigDir: filepath.Join(root, "cfg", r.ID)})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("verdict=%s gamed=%v scores=%v cost=%.4f err=%q\n%s", v.Verdict, v.GamedChecks, v.Scores, v.CostUSD, v.Error, v.Reasoning)
		return v
	}

	good := judgeOf(map[string]string{
		"greet.go":      greet + "\nfunc Farewell(name string) string { return \"Goodbye, \" + name }\n",
		"greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestFarewell(t *testing.T) {\n\tif Farewell(\"x\") != \"Goodbye, x\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	})
	if good.Error != "" || good.Verdict != task.VerdictPass || good.GamedChecks {
		t.Errorf("known-good diff judged %s: %s", good.Verdict, good.Error)
	}
	// Known-bad: wrong text, unrelated file deleted, test hard-coded to pass.
	bad := judgeOf(map[string]string{
		"greet.go":      "package demo\n\nfunc Farewell(name string) string { return \"Bye\" }\n",
		"greet_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestFarewell(t *testing.T) { t.Skip() }\n",
		"README.md":     "unrelated\n",
	})
	if bad.Error != "" || bad.Verdict == task.VerdictPass {
		t.Errorf("known-bad diff judged %s: %s", bad.Verdict, bad.Error)
	}
	if !strings.Contains(strings.ToLower(bad.Reasoning), "greet") {
		t.Logf("reasoning does not mention the file: %s", bad.Reasoning)
	}
}

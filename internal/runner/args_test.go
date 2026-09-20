package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/100xteam-ai/foreman/internal/task"
)

func baseTask() *task.Task {
	return &task.Task{
		ID: "tsk_01J8000000000000000000TEST", Kind: task.KindCodeFix,
		Prompt:    "Fix the bug; don't use `rm -rf` $(anything) \"quoted\"",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"},
		Policy: task.Policy{
			AllowedTools: []string{"Read", "Edit", "Bash(git *)", "Bash(npm test)"},
			MaxTurns:     30, TimeoutMS: 900000, MaxCostUSD: 2.5, MaxRetries: 2,
		},
	}
}

func run(mode task.SessionMode) *task.Run {
	return &task.Run{ID: "run_x", TaskID: "tsk_x", Attempt: 1, Status: task.StatusQueued,
		SessionID: "6f049bbf-9a65-44ed-9196-a589b89878fb", SessionMode: mode}
}

func TestBuildArgsGolden(t *testing.T) {
	common := []string{
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--allowedTools", "Read,Edit,Bash(git *),Bash(npm test)",
		"--max-turns", "30", "--max-budget-usd", "2.25", "--permission-mode", "dontAsk",
		"--setting-sources", "project", "--strict-mcp-config",
	}
	prompt := baseTask().Prompt
	cases := []struct {
		name string
		mod  func(*task.Task, *task.Run)
		opts ArgsOptions
		want []string
	}{
		{"new", func(*task.Task, *task.Run) {}, ArgsOptions{},
			append(append([]string{"-p", prompt}, common...), "--session-id", "6f049bbf-9a65-44ed-9196-a589b89878fb")},
		{"continue", func(_ *task.Task, r *task.Run) { r.SessionMode = task.SessionContinue }, ArgsOptions{},
			append(append([]string{"-p", prompt}, common...), "--resume", "6f049bbf-9a65-44ed-9196-a589b89878fb")},
		{"fork", func(_ *task.Task, r *task.Run) {
			r.SessionMode = task.SessionFork
			r.ParentSessionID = "d1ba9815-1ddf-4c2c-bdfc-a673f5a69b64"
		}, ArgsOptions{},
			append(append([]string{"-p", prompt}, common...), "--resume", "d1ba9815-1ddf-4c2c-bdfc-a673f5a69b64", "--fork-session", "--session-id", "6f049bbf-9a65-44ed-9196-a589b89878fb")},
		{"ephemeral", func(_ *task.Task, r *task.Run) { r.SessionMode = task.SessionEphemeral }, ArgsOptions{},
			append(append([]string{"-p", prompt}, common...), "--session-id", "6f049bbf-9a65-44ed-9196-a589b89878fb", "--no-session-persistence")},
		{"schema+model+stdin", func(tk *task.Task, _ *task.Run) {
			tk.Acceptance.JSONSchema = json.RawMessage(`{"type":"object"}`)
			tk.Policy.Model = "claude-sonnet-5"
		}, ArgsOptions{PromptViaStdin: true},
			append(append([]string{"-p"}, common...), "--json-schema", `{"type":"object"}`, "--model", "claude-sonnet-5", "--session-id", "6f049bbf-9a65-44ed-9196-a589b89878fb")},
		{"bare", func(*task.Task, *task.Run) {}, ArgsOptions{Bare: true, AddDirs: []string{"/ws"}, BudgetFlagRatio: 1},
			[]string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--include-partial-messages",
				"--allowedTools", "Read,Edit,Bash(git *),Bash(npm test)", "--max-turns", "30", "--max-budget-usd", "2.5",
				"--permission-mode", "dontAsk", "--bare", "--add-dir", "/ws", "--session-id", "6f049bbf-9a65-44ed-9196-a589b89878fb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk, r := baseTask(), run(task.SessionNew)
			tc.mod(tk, r)
			got, err := BuildArgs(tk, r, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("args differ\n got %q\nwant %q", got, tc.want)
			}
			for _, a := range got {
				if a == "--permission-prompts" {
					t.Error("flag does not exist in CLI 2.1.243")
				}
			}
		})
	}
}

func TestBuildArgsRejectsIllegalSessions(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*task.Run)
	}{
		{"fork without parent", func(r *task.Run) { r.SessionMode = task.SessionFork }},
		{"fork reusing parent id", func(r *task.Run) { r.SessionMode = task.SessionFork; r.ParentSessionID = r.SessionID }},
		{"continue with parent (session-id + resume without fork)", func(r *task.Run) {
			r.SessionMode = task.SessionContinue
			r.ParentSessionID = "d1ba9815-1ddf-4c2c-bdfc-a673f5a69b64"
		}},
		{"new with parent", func(r *task.Run) { r.ParentSessionID = "d1ba9815-1ddf-4c2c-bdfc-a673f5a69b64" }},
		{"non-uuid session id", func(r *task.Run) { r.SessionID = "sess-1" }},
		{"unknown mode", func(r *task.Run) { r.SessionMode = "weird" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := run(task.SessionNew)
			tc.mod(r)
			_, err := BuildArgs(baseTask(), r, ArgsOptions{})
			if !errors.Is(err, ErrIllegalSession) {
				t.Errorf("want ErrIllegalSession, got %v", err)
			}
		})
	}
}

func TestBuildArgsRejectsBadPolicyAndRatio(t *testing.T) {
	tk := baseTask()
	tk.Policy.MaxTurns = 0
	if _, err := BuildArgs(tk, run(task.SessionNew), ArgsOptions{}); err == nil {
		t.Error("invalid policy accepted")
	}
	if _, err := BuildArgs(baseTask(), run(task.SessionNew), ArgsOptions{BudgetFlagRatio: 1.5}); err == nil {
		t.Error("ratio > 1 accepted")
	}
}

func TestFormatUSD(t *testing.T) {
	for in, want := range map[float64]string{2.25: "2.25", 2.5: "2.5", 0.05: "0.05", 0.045: "0.045", 3: "3", 0.00001: "0"} {
		if got := formatUSD(in); got != want {
			t.Errorf("formatUSD(%v) = %q want %q", in, got, want)
		}
	}
}

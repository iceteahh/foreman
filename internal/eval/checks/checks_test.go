package checks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/runner"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/workspace"
)

func fixtureResult(t *testing.T, name string) *runner.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	ev := events.Parse(b, 1)
	res := &runner.Result{Result: ev.Result, Outcome: events.Classify(ev.Result)}
	if res.Result != nil && res.Result.IsError {
		res.ExitCode = 1
	}
	return res
}

func newTask(kind task.Kind) *task.Task {
	return &task.Task{ID: "tsk_01J8000000000000000000TEST", Kind: kind, Prompt: "p",
		Workspace:  task.WorkspaceSpec{Type: "git", Repo: "x", Ref: "main"},
		Policy:     task.Policy{AllowedTools: []string{"Read", "Edit"}, MaxTurns: 30, TimeoutMS: 1000, MaxCostUSD: 1, CriticalTools: []string{"Edit", "Bash(npm test)"}},
		Acceptance: task.Acceptance{DiffScope: []string{"src/**"}}}
}

// provision clones a fixture repo and returns the workspace plus a writer for edits.
func provision(t *testing.T, tk *task.Task, files map[string]string) (workspace.Workspace, func(name, body string)) {
	t.Helper()
	origin := workspace.NewBareRepo(t, files)
	tk.Workspace.Repo = origin
	mgr, err := workspace.NewLocal(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := mgr.Provision(context.Background(), tk, task.NewRun(tk, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Destroy() })
	write := func(name, body string) {
		p := filepath.Join(ws.Path(), name)
		if body == "" {
			_ = os.Remove(p)
			return
		}
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return ws, write
}

var baseFiles = map[string]string{
	"README.md":     "# x\n",
	"src/a.go":      "package a\n",
	"src/a_test.go": "package a\n// t1\n// t2\n// t3\n",
	"pass.sh":       "#!/bin/sh\nexit 0\n",
	"fail.sh":       "#!/bin/sh\necho boom >&2; exit 2\n",
}

func TestHappyPath(t *testing.T) {
	tk := newTask(task.KindCodeFix)
	tk.Acceptance.Commands = []string{"sh pass.sh", "sh -c 'echo ok'"}
	ws, write := provision(t, tk, baseFiles)
	write("src/a.go", "package a\n\nfunc A() {}\n")
	in := &Input{Task: tk, Result: fixtureResult(t, "result_success.json"), Workspace: ws}
	rep, err := Run(context.Background(), in, Default()...)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("expected pass:\n%s", rep.Markdown())
	}
	want := map[string]Status{"exit_code": Pass, "turn_exhaustion": Pass, "permission_denials": Pass, "acceptance_commands": Pass,
		"diff_scope": Pass, "diff_sanity": Pass, "schema_validation": NA}
	for name, st := range want {
		if got, _ := rep.Get(name); got.Status != st {
			t.Errorf("%s = %s want %s (%s)", name, got.Status, st, got.Evidence)
		}
	}
	if len(in.Outputs) != 2 {
		t.Errorf("outputs %d", len(in.Outputs))
	}
	if s := rep.Summary(); !strings.Contains(s, "pass: acceptance_commands") || !strings.Contains(s, "n/a: schema_validation") {
		t.Errorf("summary %q", s)
	}
}

func TestExitCodeAndTurns(t *testing.T) {
	tk := newTask(task.KindCodeFix)
	cases := []struct {
		name   string
		res    *runner.Result
		exit   Status
		turns  Status
		reason string
	}{
		{"nil result", nil, Fail, NA, "no result"},
		{"crash", &runner.Result{Outcome: events.OutcomeCrash, ExitCode: 137, Signaled: true, Stderr: "killed"}, Fail, NA, "without a result event"},
		{"timeout", &runner.Result{Outcome: events.OutcomeCrash, ExitCode: 143, Killed: runner.KillTimeout}, Fail, NA, "killed=timeout"},
		{"budget", fixtureResult(t, "result_budget_exhausted.json"), Fail, Pass, "budget_exhausted"},
		{"auth", fixtureResult(t, "result_not_logged_in.json"), Fail, Pass, "api_error"},
		{"ok", fixtureResult(t, "result_success.json"), Pass, Pass, "completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &Input{Task: tk, Result: tc.res}
			e := (ExitCode{}).Run(context.Background(), in)
			if e.Status != tc.exit || !strings.Contains(e.Evidence, tc.reason) {
				t.Errorf("exit_code = %s %q", e.Status, e.Evidence)
			}
			if tr := (TurnExhaustion{}).Run(context.Background(), in); tr.Status != tc.turns {
				t.Errorf("turn_exhaustion = %s %q", tr.Status, tr.Evidence)
			}
		})
	}
	// Turn exhaustion when num_turns hits the cap, and when the CLI says max_turns.
	res := fixtureResult(t, "result_success.json")
	tk.Policy.MaxTurns = 1
	if r := (TurnExhaustion{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail {
		t.Errorf("num_turns==max_turns should fail: %+v", r)
	}
	tk.Policy.MaxTurns = 30
	res.Outcome = events.OutcomeMaxTurns
	if r := (TurnExhaustion{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail {
		t.Errorf("max_turns outcome should fail: %+v", r)
	}
	// Completed result but non-zero exit is unhealthy.
	res = fixtureResult(t, "result_success.json")
	res.ExitCode = 2
	if r := (ExitCode{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail {
		t.Error("completed + exit 2 should fail")
	}
}

func TestPermissionDenials(t *testing.T) {
	tk := newTask(task.KindCodeFix)
	mk := func(tools ...string) *runner.Result {
		r := fixtureResult(t, "result_success.json")
		for _, tool := range tools {
			r.Result.PermissionDenials = append(r.Result.PermissionDenials, events.PermissionDenial{Tool: tool, Input: json.RawMessage(`{"command":"rm -rf /"}`)})
		}
		return r
	}
	if r := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: mk()}); r.Status != Pass {
		t.Errorf("no denials: %+v", r)
	}
	if r := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: mk("WebFetch")}); r.Status != Pass || !strings.Contains(r.Evidence, "WebFetch") {
		t.Errorf("non-critical denial: %+v", r)
	}
	if r := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: mk("WebFetch", "Edit")}); r.Status != Fail || !strings.Contains(r.Evidence, "task-critical tool(s): Edit") {
		t.Errorf("critical denial: %+v", r)
	}
	// Bash(npm test) in the critical list matches a denied "Bash".
	if r := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: mk("Bash")}); r.Status != Fail {
		t.Errorf("Bash base-name match: %+v", r)
	}
	if r := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: &runner.Result{}}); r.Status != NA {
		t.Errorf("no result: %+v", r)
	}
}

func TestAcceptanceCommands(t *testing.T) {
	tk := newTask(task.KindCodeFix)
	tk.Acceptance.Commands = []string{"sh pass.sh", "sh fail.sh", "'unterminated"}
	ws, _ := provision(t, tk, baseFiles)
	in := &Input{Task: tk, Workspace: ws, Outputs: map[string]workspace.ExecResult{}}
	r := (AcceptanceCommands{}).Run(context.Background(), in)
	if r.Status != Fail || !strings.Contains(r.Evidence, "2 of 3") || !strings.Contains(r.Evidence, "boom") || !strings.Contains(r.Evidence, "cannot parse") {
		t.Errorf("%+v", r)
	}
	if in.Outputs["sh fail.sh"].ExitCode != 2 {
		t.Errorf("outputs not captured: %+v", in.Outputs)
	}
	// Timeout is reported.
	tk.Acceptance.Commands = []string{"sleep 5"}
	in = &Input{Task: tk, Workspace: ws, CommandTimeout: 200 * time.Millisecond, Outputs: map[string]workspace.ExecResult{}}
	if r := (AcceptanceCommands{}).Run(context.Background(), in); r.Status != Fail || !strings.Contains(r.Evidence, "TIMED OUT") {
		t.Errorf("timeout: %+v", r)
	}
	// Shell operators are refused outright (no shell): nothing after ';' may run.
	tk.Acceptance.Commands = []string{"cat README.md;echo INJECTED", "true | false", "echo x > pwned"}
	in = &Input{Task: tk, Workspace: ws, Outputs: map[string]workspace.ExecResult{}}
	if r := (AcceptanceCommands{}).Run(context.Background(), in); r.Status != Fail || !strings.Contains(r.Evidence, "3 of 3") || !strings.Contains(r.Evidence, "shell operators") || len(in.Outputs) != 0 {
		t.Errorf("shell injection: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(ws.Path(), "pwned")); err == nil {
		t.Error("redirect was executed")
	}
	if r := (AcceptanceCommands{}).Run(context.Background(), &Input{Task: newTask(task.KindCodeFix)}); r.Status != NA {
		t.Errorf("no commands: %+v", r)
	}
}

func TestDiffScopeAndSanity(t *testing.T) {
	ctx := context.Background()
	t.Run("out of scope", func(t *testing.T) {
		tk := newTask(task.KindCodeFix)
		ws, write := provision(t, tk, baseFiles)
		write("src/a.go", "package a\n// changed\n")
		write("README.md", "# changed\n")
		rep, _ := Run(ctx, &Input{Task: tk, Workspace: ws}, DiffScope{}, DiffSanity{})
		ds, _ := rep.Get("diff_scope")
		if ds.Status != Fail || !strings.Contains(ds.Evidence, "README.md") || strings.Contains(ds.Evidence, "src/a.go\n") {
			t.Errorf("%+v", ds)
		}
		if s, _ := rep.Get("diff_sanity"); s.Status != Pass {
			t.Errorf("%+v", s)
		}
	})
	t.Run("empty diff on change task", func(t *testing.T) {
		tk := newTask(task.KindCodeFix)
		ws, _ := provision(t, tk, baseFiles)
		rep, _ := Run(ctx, &Input{Task: tk, Workspace: ws}, DiffScope{}, DiffSanity{})
		if s, _ := rep.Get("diff_sanity"); s.Status != Fail || !strings.Contains(s.Evidence, "empty diff") {
			t.Errorf("%+v", s)
		}
		if ds, _ := rep.Get("diff_scope"); ds.Status != Pass {
			t.Errorf("%+v", ds)
		}
	})
	t.Run("read-only task changed files", func(t *testing.T) {
		tk := newTask(task.KindCodeReview)
		ws, write := provision(t, tk, baseFiles)
		write("src/a.go", "package a\n// changed\n")
		rep, _ := Run(ctx, &Input{Task: tk, Workspace: ws}, DiffSanity{})
		if s, _ := rep.Get("diff_sanity"); s.Status != Fail || !strings.Contains(s.Evidence, "read-only") {
			t.Errorf("%+v", s)
		}
	})
	t.Run("deleted and shrunk tests", func(t *testing.T) {
		tk := newTask(task.KindCodeFix)
		ws, write := provision(t, tk, map[string]string{"src/a.go": "package a\n", "src/a_test.go": "package a\n// t1\n// t2\n// t3\n", "src/b_test.go": "package a\n"})
		write("src/a_test.go", "package a\n")
		write("src/b_test.go", "")
		write("src/a.go", "package a\n// fix\n")
		rep, _ := Run(ctx, &Input{Task: tk, Workspace: ws}, DiffSanity{})
		s, _ := rep.Get("diff_sanity")
		if s.Status != Fail || !strings.Contains(s.Evidence, "deleted test file src/b_test.go") || !strings.Contains(s.Evidence, "src/a_test.go shrank") {
			t.Errorf("%+v", s)
		}
	})
	t.Run("no scope configured", func(t *testing.T) {
		tk := newTask(task.KindCodeFix)
		tk.Acceptance.DiffScope = nil
		if r := (DiffScope{}).Run(ctx, &Input{Task: tk, Capture: &workspace.Capture{ChangedFiles: []string{"x"}}}); r.Status != NA {
			t.Errorf("%+v", r)
		}
	})
}

func TestIsTestFile(t *testing.T) {
	for p, want := range map[string]bool{
		"src/a_test.go": true, "lib/x.test.ts": true, "lib/x.spec.js": true, "tests/unit/foo.py": true, "pkg/__tests__/a.js": true,
		"test_models.py": true, "FooTest.java": true, "src/a.go": false, "testdata/fixture.json": false, "contest.go": false,
	} {
		if got := IsTestFile(p); got != want {
			t.Errorf("IsTestFile(%q) = %v", p, got)
		}
	}
}

func TestSchemaValidation(t *testing.T) {
	tk := newTask(task.KindReport)
	schema := json.RawMessage(`{"type":"object","properties":{"verdict":{"enum":["pass","fail"]},"n":{"type":"integer"}},"required":["verdict"]}`)
	if r := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: fixtureResult(t, "result_success.json")}); r.Status != NA {
		t.Errorf("no schema: %+v", r)
	}
	tk.Acceptance.JSONSchema = schema
	res := fixtureResult(t, "result_success.json")
	if r := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail || !strings.Contains(r.Evidence, "no structured_output") {
		t.Errorf("missing output: %+v", r)
	}
	res.Result.StructuredOutput = json.RawMessage(`{"verdict":"pass","n":3}`)
	if r := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Pass {
		t.Errorf("valid: %+v", r)
	}
	res.Result.StructuredOutput = json.RawMessage(`{"verdict":"maybe","n":"x"}`)
	if r := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail || !strings.Contains(r.Evidence, "verdict") {
		t.Errorf("invalid: %+v", r)
	}
	tk.Acceptance.JSONSchema = json.RawMessage(`{"type":"nonsense"}`)
	if r := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: res}); r.Status != Fail || !strings.Contains(r.Evidence, "compile") {
		t.Errorf("bad schema: %+v", r)
	}
}

func TestMarkdown(t *testing.T) {
	rep := Report{Results: []Result{{Name: "exit_code", Status: Pass, Evidence: "fine"}, {Name: "diff_scope", Status: Fail, Evidence: "README.md"}, {Name: "schema_validation", Status: NA}}}
	md := rep.Markdown()
	if !strings.Contains(md, "| exit_code | ✅ pass |") || !strings.Contains(md, "<summary><code>diff_scope</code> — fail</summary>") || strings.Contains(md, "fine") {
		t.Errorf("%s", md)
	}
	out := rep.Outcomes()
	if out["diff_scope"].Status != "fail" || out["diff_scope"].Evidence != "README.md" || len(out) != 3 {
		t.Errorf("%+v", out)
	}
}

func TestExpectChangesOverrideAndFromOutcomes(t *testing.T) {
	ctx := context.Background()
	// A read-only phase of a change kind: no diff passes, a diff fails.
	tk := newTask(task.KindCodeFixPlanned)
	f := false
	tk.Acceptance.ExpectChanges = &f
	ws, write := provision(t, tk, map[string]string{"src/a.go": "package a\n"})
	res := fixtureResult(t, "result_structured_output.json")
	rep, err := Run(ctx, &Input{Task: tk, Run: task.NewRun(tk, time.Now()), Result: res, Workspace: ws}, DiffSanity{})
	if err != nil || rep.Results[0].Status != Pass || !strings.Contains(rep.Results[0].Evidence, "read-only") {
		t.Errorf("%+v %v", rep.Results, err)
	}
	write("src/a.go", "package a\n// changed\n")
	rep, _ = Run(ctx, &Input{Task: tk, Run: task.NewRun(tk, time.Now()), Result: res, Workspace: ws}, DiffSanity{})
	if rep.Results[0].Status != Fail || !strings.Contains(rep.Results[0].Evidence, "read-only task modified 1 file") {
		t.Errorf("%+v", rep.Results[0])
	}
	// A change phase of a read-only kind must change something.
	tk2 := newTask(task.KindCodeReview)
	tr := true
	tk2.Acceptance.ExpectChanges = &tr
	ws2, _ := provision(t, tk2, map[string]string{"src/a.go": "package a\n"})
	rep, _ = Run(ctx, &Input{Task: tk2, Run: task.NewRun(tk2, time.Now()), Result: res, Workspace: ws2}, DiffSanity{})
	if rep.Results[0].Status != Fail || !strings.Contains(rep.Results[0].Evidence, "empty diff") {
		t.Errorf("%+v", rep.Results[0])
	}

	// FromOutcomes restores Default() order and appends unknown names.
	m := map[string]task.CheckOutcome{"schema_validation": {Status: "n/a"}, "exit_code": {Status: "pass", Evidence: "ok"}, "custom_x": {Status: "fail", Evidence: "boom"}, "diff_scope": {Status: "fail"}}
	got := FromOutcomes(m)
	names := make([]string, 0, len(got.Results))
	for _, r := range got.Results {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "exit_code,diff_scope,schema_validation,custom_x" || !got.Failed() || strings.Join(got.FailedNames(), ",") != "diff_scope,custom_x" {
		t.Errorf("%v failed=%v", names, got.FailedNames())
	}
	if got.Results[0].Evidence != "ok" || got.Outcomes()["custom_x"].Evidence != "boom" {
		t.Errorf("%+v", got.Results)
	}
}

func TestPermissionDenialFixture(t *testing.T) {
	// Real denial (CLI 2.1.270): Write and Bash denied under dontAsk with only Read allowed.
	res := fixtureResult(t, "result_permission_denial.json")
	tk := newTask(task.KindCodeFix) // critical: Edit, Bash(npm test)
	rep := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: res})
	if rep.Status != Fail || !strings.Contains(rep.Evidence, "denied task-critical tool(s): Bash") || !strings.Contains(rep.Evidence, "rm -f hello.txt") {
		t.Errorf("%+v", rep)
	}
	tk.Policy.CriticalTools = []string{"Edit"}
	if rep := (PermissionDenials{}).Run(context.Background(), &Input{Task: tk, Result: res}); rep.Status != Pass || !strings.Contains(rep.Evidence, "2 denial(s)") {
		t.Errorf("%+v", rep)
	}
	// Real max_turns exhaustion: num_turns (2) exceeds --max-turns 1; exit_code fails too.
	mt := fixtureResult(t, "result_max_turns.json")
	tk.Policy.MaxTurns = 1
	if rep := (TurnExhaustion{}).Run(context.Background(), &Input{Task: tk, Result: mt}); rep.Status != Fail || !strings.Contains(rep.Evidence, "num_turns=2 max_turns=1") {
		t.Errorf("%+v", rep)
	}
	if rep := (ExitCode{}).Run(context.Background(), &Input{Task: tk, Result: mt}); rep.Status != Fail || !strings.Contains(rep.Evidence, "terminal_reason=max_turns") {
		t.Errorf("%+v", rep)
	}
	// Real structured output validates against the schema it was produced with.
	so := fixtureResult(t, "result_structured_output.json")
	tk.Acceptance.JSONSchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"steps":{"type":"array","items":{"type":"string"}},"risk":{"enum":["low","medium","high"]}},"required":["summary","steps","risk"]}`)
	if rep := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: so}); rep.Status != Pass {
		t.Errorf("%+v", rep)
	}
	tk.Acceptance.JSONSchema = json.RawMessage(`{"type":"object","required":["nope"]}`)
	if rep := (SchemaValidation{}).Run(context.Background(), &Input{Task: tk, Result: so}); rep.Status != Fail {
		t.Errorf("%+v", rep)
	}
}

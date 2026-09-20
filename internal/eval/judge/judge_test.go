package judge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/runner"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/workspace"
)

func fixtureResult(t *testing.T, name string) *events.ResultEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	ev := events.Parse(b, 1)
	if ev.Result == nil {
		t.Fatalf("%s is not a result event", name)
	}
	return ev.Result
}

type fakeSpawner struct {
	mu      sync.Mutex
	tasks   []*task.Task
	runs    []*task.Run
	spawns  []runner.Spawn
	results []*runner.Result
}

func (f *fakeSpawner) Run(_ context.Context, t *task.Task, r *task.Run, sp runner.Spawn) (*runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, t)
	f.runs = append(f.runs, r)
	f.spawns = append(f.spawns, sp)
	i := len(f.tasks) - 1
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i], nil
}

func completed(structured string) *runner.Result {
	return &runner.Result{Outcome: events.OutcomeCompleted, ExitCode: 0, EventLogURI: "file:///log",
		Result: &events.ResultEvent{Subtype: "success", TerminalReason: "completed", TotalCostUSD: 0.02, NumTurns: 1, StructuredOutput: json.RawMessage(structured)}}
}

func rubric(verdict string, min int, gamed bool) string {
	return `{"task_completion":` + itoa(min) + `,"minimal_diff":9,"no_scope_creep":9,"code_quality":8,"gamed_checks":` + boolStr(gamed) + `,"verdict":"` + verdict + `","reasoning":"because"}`
}

func itoa(i int) string     { return json.Number(strings.TrimSpace(string(mustJSON(i)))).String() }
func boolStr(b bool) string { return string(mustJSON(b)) }
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func input() *Input {
	tk := &task.Task{ID: task.NewTaskID(), Kind: task.KindCodeFix, Prompt: "Add Farewell to greet.go", Title: "farewell",
		Workspace:  task.WorkspaceSpec{Type: "git", Repo: "o/r", Ref: "main"},
		Policy:     task.Policy{AllowedTools: []string{"Read", "Edit"}, MaxTurns: 10, TimeoutMS: 60000, MaxCostUSD: 1, Judge: task.JudgePolicy{Enabled: true, Samples: 1, Threshold: 7}},
		Acceptance: task.Acceptance{Commands: []string{"go test ./..."}, DiffScope: []string{"*.go"}}}
	r := task.NewRun(tk, tk.CreatedAt)
	r.ID = "run_01TESTJUDGE"
	rep := checks.Report{Results: []checks.Result{{Name: "exit_code", Status: checks.Pass, Evidence: "ok"}, {Name: "acceptance_commands", Status: checks.Pass, Evidence: "$ go test ./...\nok"}}}
	res := completed("")
	res.Result.Result = ptr("Added Farewell and a test.")
	return &Input{Task: tk, Run: r, Checks: rep, Result: res, Workspace: "/ws", ConfigDir: "/cfg/run_x",
		Capture: &workspace.Capture{BaseSHA: "abc123", ChangedFiles: []string{"greet.go", "greet_test.go"}, Diff: "diff --git a/greet.go b/greet.go\n+func Farewell() {}\n"},
		Outputs: map[string]workspace.ExecResult{"go test ./...": {ExitCode: 0, Stdout: "ok  demo 0.1s"}}}
}

func ptr(s string) *string { return &s }

func TestParseResultFixtures(t *testing.T) {
	// The real structured-output result shape: structured_output is an object and result.result mirrors it as text.
	res := &runner.Result{Outcome: events.OutcomeCompleted, Result: fixtureResult(t, "result_structured_output.json")}
	s := ParseResult(res)
	if s.Valid() || !strings.Contains(s.Error, "does not match the rubric") {
		t.Errorf("a plan is not a verdict: %+v", s)
	}
	for _, tc := range []struct {
		name    string
		res     *runner.Result
		wantErr string
	}{
		{"budget", &runner.Result{Outcome: events.OutcomeBudgetExhausted, Result: fixtureResult(t, "result_budget_exhausted.json")}, "did not complete"},
		{"max_turns", &runner.Result{Outcome: events.OutcomeMaxTurns, Result: fixtureResult(t, "result_max_turns.json")}, "did not complete"},
		{"api_error", &runner.Result{Outcome: events.OutcomeAPIError, Result: fixtureResult(t, "result_not_logged_in.json")}, "did not complete"},
		{"crash", &runner.Result{Outcome: events.OutcomeCrash}, "without a result"},
		{"killed", &runner.Result{Outcome: events.OutcomeCrash, Killed: runner.KillTimeout}, "judge killed: timeout"},
		{"nil", nil, "no result"},
		{"plain text", func() *runner.Result { r := completed(""); r.Result.Result = ptr("looks fine"); return r }(), "no structured_output"},
		{"bad verdict", completed(`{"verdict":"maybe","reasoning":"x","gamed_checks":false}`), "does not match the rubric"},
		{"score range", completed(`{"verdict":"pass","reasoning":"x","gamed_checks":false,"task_completion":11}`), "does not match the rubric"},
	} {
		s := ParseResult(tc.res)
		if s.Valid() || s.Verdict != task.VerdictUncertain || !strings.Contains(s.Error, tc.wantErr) {
			t.Errorf("%s: %+v", tc.name, s)
		}
	}
	good := ParseResult(completed(rubric("pass", 8, false)))
	if !good.Valid() || good.Verdict != "pass" || good.Scores["task_completion"] != 8 || good.Scores["code_quality"] != 8 || good.Reasoning != "because" || good.CostUSD != 0.02 {
		t.Errorf("%+v", good)
	}
	// result.result carrying the JSON is accepted when structured_output is absent.
	txt := completed("")
	txt.Result.Result = ptr(rubric("fail", 2, false))
	if s := ParseResult(txt); !s.Valid() || s.Verdict != "fail" {
		t.Errorf("%+v", s)
	}
	if s := ParseResult(completed(rubric("pass", 9, true))); s.Verdict != "fail" || !s.GamedChecks {
		t.Errorf("gamed must fail: %+v", s)
	}
}

func TestAggregate(t *testing.T) {
	v := func(verdict string, min int, gamed bool) Sample {
		return ParseResult(completed(rubric(verdict, min, gamed)))
	}
	bad := Sample{Verdict: task.VerdictUncertain, Error: "boom", CostUSD: 0.01}
	cases := []struct {
		name    string
		samples []Sample
		verdict string
		gamed   bool
		min     int
		hasMin  bool
	}{
		{"single pass", []Sample{v("pass", 8, false)}, "pass", false, 8, true},
		{"majority pass", []Sample{v("pass", 9, false), v("fail", 3, false), v("pass", 7, false)}, "pass", false, 6, true},
		{"majority fail", []Sample{v("fail", 2, false), v("fail", 3, false), v("pass", 9, false)}, "fail", false, 5, true},
		{"tie is uncertain", []Sample{v("pass", 9, false), v("fail", 2, false)}, "uncertain", false, 6, true},
		{"one gamed of three", []Sample{v("pass", 9, true), v("pass", 9, false), v("pass", 9, false)}, "pass", false, 8, true},
		{"two gamed of three", []Sample{v("pass", 9, true), v("pass", 9, true), v("pass", 9, false)}, "fail", true, 8, true},
		{"invalid ignored", []Sample{bad, v("pass", 8, false)}, "pass", false, 8, true},
		{"all invalid", []Sample{bad, bad}, "uncertain", false, 0, false},
		{"uncertain majority", []Sample{v("uncertain", 5, false), v("uncertain", 6, false), v("pass", 9, false)}, "uncertain", false, 7, true},
	}
	for _, tc := range cases {
		got := Aggregate(tc.samples)
		min, ok := got.MinScore()
		if got.Verdict != tc.verdict || got.GamedChecks != tc.gamed || ok != tc.hasMin || (ok && min != tc.min) {
			t.Errorf("%s: verdict=%s gamed=%v min=%d/%v want %s/%v/%d/%v", tc.name, got.Verdict, got.GamedChecks, min, ok, tc.verdict, tc.gamed, tc.min, tc.hasMin)
		}
		if len(got.Samples) != len(tc.samples) {
			t.Errorf("%s: samples lost", tc.name)
		}
	}
	all := Aggregate([]Sample{bad, bad})
	if all.Error == "" || !strings.Contains(all.Reasoning, "judge unavailable") || all.CostUSD != 0.02 || all.ToTask().Error == "" {
		t.Errorf("%+v", all)
	}
	if u := Uncertain("x"); u.Verdict != "uncertain" || u.Failed() || u.ToTask().Error != "x" {
		t.Errorf("%+v", u)
	}
	if !Aggregate([]Sample{v("fail", 1, false)}).Failed() || Aggregate([]Sample{v("uncertain", 5, false)}).Failed() {
		t.Error("Failed()")
	}
}

func TestPromptContents(t *testing.T) {
	in := input()
	p, err := Prompt(in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Add Farewell to greet.go", "`go test ./...`", "`*.go`", "The task must change the workspace", "pass: acceptance_commands, exit_code",
		"ok  demo 0.1s", "Diff against base abc123 (2 file(s))", "+func Farewell() {}", "Worker's final message", "Added Farewell and a test.", "gamed_checks must be true"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "Structured output") || strings.Contains(p, "### exit_code") {
		t.Error("prompt shows sections it should not")
	}
	// Failed check evidence and truncation.
	in.Checks.Results = append(in.Checks.Results, checks.Result{Name: "diff_scope", Status: checks.Fail, Evidence: "out-of-scope file: README.md"})
	in.Capture.Diff = strings.Repeat("x", 100)
	in.Result.Result.StructuredOutput = json.RawMessage(`{"a":1}`)
	in.Result.Result.Result = ptr(`{"a":1}`)
	p, _ = Prompt(in, 10)
	if !strings.Contains(p, "### diff_scope — fail") || !strings.Contains(p, "out-of-scope file: README.md") || !strings.Contains(p, "diff truncated") || !strings.Contains(p, "xxxxxxxxxx\n```") {
		t.Errorf("prompt:\n%s", p)
	}
	if !strings.Contains(p, "Worker's structured output") || strings.Contains(p, "Worker's final message") {
		t.Error("structured output mirrored into result.result must be shown once")
	}
	// Read-only task, no diff.
	f := false
	in.Task.Acceptance.ExpectChanges = &f
	in.Capture = nil
	p, _ = Prompt(in, 0)
	if !strings.Contains(p, "The task is read-only") || !strings.Contains(p, "(no changes)") {
		t.Errorf("prompt:\n%s", p)
	}
}

func TestEvaluateIsolationAndSamples(t *testing.T) {
	in := input()
	in.Task.Policy.Judge.Samples = 3
	in.Task.Policy.Judge.Model = "haiku"
	fs := &fakeSpawner{results: []*runner.Result{completed(rubric("pass", 9, false)), completed(rubric("pass", 8, false)), completed(rubric("fail", 2, false))}}
	j := &Judge{Spawner: fs, Options: Options{MaxTurns: 4, MaxCostUSD: 0.2}}
	v, err := j.Evaluate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "pass" || len(v.Samples) != 3 || v.CostUSD < 0.059 || v.CostUSD > 0.061 || v.Scores["task_completion"] != 6 {
		t.Errorf("%+v", v)
	}
	if len(fs.tasks) != 3 {
		t.Fatalf("spawned %d", len(fs.tasks))
	}
	seen := map[string]bool{}
	for i, jt := range fs.tasks {
		jr, sp := fs.runs[i], fs.spawns[i]
		if jt.ID != in.Task.ID || strings.Join(jt.Policy.AllowedTools, ",") != "Read" || jt.Policy.MaxTurns != 4 || jt.Policy.MaxCostUSD != 0.2 || jt.Policy.Model != "haiku" || jt.Policy.TimeoutMS != 180000 {
			t.Errorf("judge task policy %+v", jt.Policy)
		}
		if string(jt.Acceptance.JSONSchema) != string(Rubric) || jt.RequestedBy != "judge" || !strings.Contains(jt.Prompt, "independent judge") {
			t.Errorf("judge task %+v", jt)
		}
		if jr.SessionMode != task.SessionEphemeral || jr.SessionID == in.Run.SessionID || seen[jr.SessionID] || jr.ID != "run_01TESTJUDGE-judge-"+itoa(i+1) || jr.Attempt != i+1 {
			t.Errorf("judge run %+v", jr)
		}
		seen[jr.SessionID] = true
		if sp.Workspace != "/ws" || sp.ConfigDir != "/cfg/run_x-judge-"+itoa(i+1) {
			t.Errorf("spawn %+v", sp)
		}
		if _, err := runner.BuildArgs(jt, jr, runner.ArgsOptions{}); err != nil {
			t.Errorf("judge args invalid: %v", err)
		}
	}
	// Options.Model applies when the policy has none; parallel works.
	in.Task.Policy.Judge.Model = ""
	fs2 := &fakeSpawner{results: []*runner.Result{completed(rubric("pass", 9, false))}}
	j2 := &Judge{Spawner: fs2, Options: Options{Model: "sonnet", Parallel: true}}
	if _, err := j2.Evaluate(context.Background(), in); err != nil || fs2.tasks[0].Policy.Model != "sonnet" || len(fs2.tasks) != 3 {
		t.Errorf("%v %+v", err, fs2.tasks)
	}
	if _, err := (&Judge{}).Evaluate(context.Background(), in); err == nil {
		t.Error("missing spawner accepted")
	}
	if _, err := j.Evaluate(context.Background(), &Input{Task: in.Task, Run: in.Run}); err == nil {
		t.Error("missing workspace accepted")
	}
}

func TestRubricIsValidSchema(t *testing.T) {
	if err := checks.ValidateJSON(Rubric, []byte(rubric("pass", 5, false))); err != nil {
		t.Fatal(err)
	}
	if err := checks.ValidateJSON(Rubric, []byte(`{"verdict":"pass"}`)); err == nil {
		t.Error("missing required fields accepted")
	}
}

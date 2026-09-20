package golden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

func run(status task.RunStatus, mod func(*task.Run)) *task.Run {
	r := &task.Run{ID: "run_1", TaskID: "tsk_1", Attempt: 1, Status: status,
		Metrics: task.Metrics{Turns: 6, CostUSD: 0.05, DurationMS: 12000}}
	if mod != nil {
		mod(r)
	}
	return r
}

func judged(verdict string, cost float64) func(*task.Run) {
	return func(r *task.Run) {
		r.Eval = &task.Eval{Checks: map[string]task.CheckOutcome{"exit_code": {Status: "pass"}},
			Judge: &task.JudgeVerdict{Verdict: verdict, CostUSD: cost}}
	}
}

func TestOutcomeOfMapsEveryStatus(t *testing.T) {
	for status, want := range map[task.RunStatus]Outcome{
		task.StatusDelivered:   OutcomePass,
		task.StatusPassed:      OutcomePass,
		task.StatusDead:        OutcomeFail,
		task.StatusFailed:      OutcomeFail,
		task.StatusClosed:      OutcomeFail,
		task.StatusNeedsReview: OutcomeNeedsReview,
		task.StatusQueued:      OutcomeError,
		task.StatusRunning:     OutcomeError,
		task.StatusEvaluating:  OutcomeError,
	} {
		if got := OutcomeOf(run(status, nil)); got != want {
			t.Errorf("%s → %s, want %s", status, got, want)
		}
	}
	if got := OutcomeOf(nil); got != OutcomeError {
		t.Errorf("nil run → %s", got)
	}
}

func TestScorePassingCase(t *testing.T) {
	c := &Case{ID: "x", Kind: task.KindCodeFix, Expect: Expect{
		Outcome: OutcomePass, FilesTouched: []string{"*.go"}, FilesUntouched: []string{"go.mod"}, MaxAttempts: 1}}
	r := run(task.StatusDelivered, judged(task.VerdictPass, 0.01))
	res := Score(c, &Execution{Task: &task.Task{ID: "tsk_1"}, Runs: []*task.Run{r}, Final: r, ChangedFiles: []string{"main.go"}})
	if !res.OK {
		t.Fatalf("expected ok, reasons %v", res.Reasons)
	}
	if res.Attempts != 1 || res.Turns != 6 {
		t.Errorf("attempts %d turns %d", res.Attempts, res.Turns)
	}
	if res.CostUSD != 0.06 { // worker 0.05 + judge 0.01
		t.Errorf("cost %v, want the judge's spend included", res.CostUSD)
	}
	if res.JudgeAgreed == nil || !*res.JudgeAgreed {
		t.Errorf("judge_agreed = %v", res.JudgeAgreed)
	}
}

func TestScoreWrongOutcome(t *testing.T) {
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass}}
	r := run(task.StatusDead, func(r *task.Run) { r.LastError = "checks failed: diff_sanity\nmore detail" })
	res := Score(c, &Execution{Final: r, Runs: []*task.Run{r}})
	if res.OK {
		t.Fatal("a dead run scored as a pass")
	}
	joined := strings.Join(res.Reasons, " | ")
	if !strings.Contains(joined, "expected pass, got fail") {
		t.Errorf("reasons %q", joined)
	}
	if !strings.Contains(joined, "checks failed: diff_sanity") {
		t.Errorf("the run's own reason is not in the report: %q", joined)
	}
}

func TestScoreFileExpectations(t *testing.T) {
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass,
		FilesTouched: []string{"ledger.go"}, FilesUntouched: []string{"*_test.go"}}}
	r := run(task.StatusDelivered, nil)

	ok := Score(c, &Execution{Final: r, Runs: []*task.Run{r}, ChangedFiles: []string{"ledger.go"}})
	if !ok.OK {
		t.Fatalf("reasons %v", ok.Reasons)
	}
	// Touched the test file: the exact gaming the case exists to catch.
	bad := Score(c, &Execution{Final: r, Runs: []*task.Run{r}, ChangedFiles: []string{"ledger.go", "ledger_test.go"}})
	if bad.OK || !strings.Contains(strings.Join(bad.Reasons, " "), "ledger_test.go") {
		t.Fatalf("test-file edit accepted: %v", bad.Reasons)
	}
	// Changed nothing at all.
	none := Score(c, &Execution{Final: r, Runs: []*task.Run{r}})
	if none.OK || !strings.Contains(strings.Join(none.Reasons, " "), "files_touched") {
		t.Fatalf("empty diff accepted: %v", none.Reasons)
	}
}

func TestScoreMaxAttempts(t *testing.T) {
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass, MaxAttempts: 1}}
	r1, r2 := run(task.StatusFailed, nil), run(task.StatusDelivered, nil)
	r2.Attempt = 2
	res := Score(c, &Execution{Final: r2, Runs: []*task.Run{r1, r2}})
	if res.OK {
		t.Fatal("a case that needed a retry scored clean")
	}
	if !strings.Contains(strings.Join(res.Reasons, " "), "took 2 attempts") {
		t.Errorf("reasons %v", res.Reasons)
	}
	if res.CostUSD != 0.1 {
		t.Errorf("cost %v, want both attempts counted", res.CostUSD)
	}
}

func TestScoreOutputSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"category":{"const":"bug"}},"required":["category"]}`)
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass, OutputSchema: schema}}

	good := run(task.StatusDelivered, func(r *task.Run) { r.Output = json.RawMessage(`{"category":"bug"}`) })
	if res := Score(c, &Execution{Final: good, Runs: []*task.Run{good}}); !res.OK {
		t.Fatalf("valid output rejected: %v", res.Reasons)
	}
	wrong := run(task.StatusDelivered, func(r *task.Run) { r.Output = json.RawMessage(`{"category":"feature"}`) })
	if res := Score(c, &Execution{Final: wrong, Runs: []*task.Run{wrong}}); res.OK {
		t.Fatal("output violating the schema accepted")
	}
	missing := run(task.StatusDelivered, nil)
	res := Score(c, &Execution{Final: missing, Runs: []*task.Run{missing}})
	if res.OK || !strings.Contains(strings.Join(res.Reasons, " "), "no structured_output") {
		t.Fatalf("missing output accepted: %v", res.Reasons)
	}
}

func TestScoreJudgeAgreement(t *testing.T) {
	for _, tc := range []struct {
		expect  Outcome
		verdict string
		agree   bool
	}{
		{OutcomePass, task.VerdictPass, true},
		{OutcomePass, task.VerdictFail, false},
		{OutcomePass, task.VerdictUncertain, false},
		{OutcomeFail, task.VerdictFail, true},
		{OutcomeFail, task.VerdictPass, false},
		{OutcomeNeedsReview, task.VerdictUncertain, true},
		{OutcomeNeedsReview, task.VerdictPass, false},
	} {
		c := &Case{ID: "x", Expect: Expect{Outcome: tc.expect}}
		got, ok := judgeAgreement(c, tc.verdict)
		if !ok || got != tc.agree {
			t.Errorf("expect %s + judge %s → %v (ok %v), want %v", tc.expect, tc.verdict, got, ok, tc.agree)
		}
	}
	// No judge ran: the case must not count towards the agreement rate.
	if _, ok := judgeAgreement(&Case{Expect: Expect{Outcome: OutcomePass}}, ""); ok {
		t.Error("an unjudged case counted towards judge agreement")
	}
}

func TestScoreExpectedJudgeVerdict(t *testing.T) {
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass, JudgeVerdict: task.VerdictPass}}
	r := run(task.StatusDelivered, judged(task.VerdictUncertain, 0))
	res := Score(c, &Execution{Final: r, Runs: []*task.Run{r}})
	if res.OK || !strings.Contains(strings.Join(res.Reasons, " "), "judge verdict") {
		t.Fatalf("reasons %v", res.Reasons)
	}
}

func TestScoreHarnessError(t *testing.T) {
	c := &Case{ID: "x", Expect: Expect{Outcome: OutcomePass}}
	res := Score(c, &Execution{Err: errors.New("provision workspace: no such host")})
	if res.OK {
		t.Fatal("a harness error scored as a pass")
	}
	if res.Actual != OutcomeError {
		t.Errorf("actual = %s, want error", res.Actual)
	}
	if !strings.Contains(res.Error, "no such host") {
		t.Errorf("error %q", res.Error)
	}
}

// ---------- report ----------

func TestSummarizeMetrics(t *testing.T) {
	yes, no := true, false
	rep := &Report{Cases: []CaseResult{
		{ID: "a", Kind: "code_fix", OK: true, Attempts: 1, Turns: 4, CostUSD: 0.10, DurationMS: 10000, JudgeAgreed: &yes},
		{ID: "b", Kind: "code_fix", OK: true, Attempts: 2, Turns: 8, CostUSD: 0.30, DurationMS: 30000, JudgeAgreed: &no},
		{ID: "c", Kind: "triage", OK: false, Attempts: 1, Turns: 2, CostUSD: 0.05, DurationMS: 5000, Actual: OutcomeError},
		{ID: "d", Kind: "triage", Skipped: true, SkipReason: "no matching tag"},
	}}
	rep.Summarize()
	tot := rep.Totals
	if tot.Cases != 4 || tot.Scored != 3 || tot.Passed != 2 || tot.Failed != 1 || tot.Skipped != 1 || tot.Errors != 1 {
		t.Fatalf("%+v", tot)
	}
	if tot.PassRate != round(2.0/3, 4) {
		t.Errorf("pass rate %v", tot.PassRate)
	}
	if tot.RetryRate != round(1.0/3, 4) {
		t.Errorf("retry rate %v", tot.RetryRate)
	}
	if tot.AvgTurns != round(14.0/3, 2) || tot.CostUSD != 0.45 {
		t.Errorf("turns %v cost %v", tot.AvgTurns, tot.CostUSD)
	}
	if tot.AvgDurationMS != 15000 {
		t.Errorf("avg duration %v", tot.AvgDurationMS)
	}
	if tot.JudgeCases != 2 || tot.JudgeAgreement != 0.5 {
		t.Errorf("judge agreement %v over %d", tot.JudgeAgreement, tot.JudgeCases)
	}
	if k := rep.ByKind["code_fix"]; k.Scored != 2 || k.PassRate != 1 {
		t.Errorf("code_fix totals %+v", k)
	}
	if k := rep.ByKind["triage"]; k.Scored != 1 || k.Skipped != 1 {
		t.Errorf("triage totals %+v", k)
	}
}

func TestReportRoundTrip(t *testing.T) {
	rep := &Report{Suite: "evals/golden", StartedAt: time.Unix(0, 0).UTC(), Cases: []CaseResult{{ID: "a", OK: true, Attempts: 1}}}
	rep.Summarize()
	p := filepath.Join(t.TempDir(), "r.json")
	if err := rep.Write(p); err != nil {
		t.Fatal(err)
	}
	back, err := ReadReport(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Totals.PassRate != 1 || len(back.Cases) != 1 || back.Cases[0].ID != "a" {
		t.Fatalf("%+v", back)
	}
	if got, ok := back.Case("a"); !ok || !got.OK {
		t.Errorf("Case(a) = %+v %v", got, ok)
	}
}

func TestDefaultReportPath(t *testing.T) {
	got := DefaultReportPath("evals/reports", time.Date(2026, 9, 16, 23, 0, 0, 0, time.UTC))
	if want := filepath.Join("evals", "reports", "2026-09-16.json"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderMentionsFailureReasons(t *testing.T) {
	rep := &Report{Cases: []CaseResult{
		{ID: "bad", Kind: "code_fix", OK: false, Actual: OutcomeFail, Reasons: []string{"expected pass, got fail"}},
		{ID: "skipped", Kind: "triage", Skipped: true, SkipReason: "cost cap reached"},
	}}
	rep.Summarize()
	var sb strings.Builder
	rep.Render(&sb)
	out := sb.String()
	for _, want := range []string{"FAIL", "bad", "expected pass, got fail", "SKIP", "cost cap reached"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// ---------- runner ----------

// fakeExecutor returns a canned execution per case, keyed by the prompt.
type fakeExecutor struct {
	byPrompt map[string]*Execution
	calls    []string
	fail     error
}

func (f *fakeExecutor) Execute(_ context.Context, spec task.Spec) (*Execution, error) {
	f.calls = append(f.calls, spec.Prompt)
	if f.fail != nil {
		return nil, f.fail
	}
	ex, ok := f.byPrompt[spec.Prompt]
	if !ok {
		return &Execution{Err: fmt.Errorf("no canned execution for %q", spec.Prompt)}, nil
	}
	return ex, nil
}

// quiet keeps the suite runner's per-case logging out of the test output.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runnerSuite writes n cases whose prompts are "case-<i>".
func runnerSuite(t *testing.T, n int, tweak func(i int) string) Suite {
	t.Helper()
	dir := suiteDir(t)
	for i := 0; i < n; i++ {
		extra := ""
		if tweak != nil {
			extra = tweak(i)
		}
		writeCase(t, dir, fmt.Sprintf("case-%d", i), fmt.Sprintf(`{
          "description":"case %d","kind":"code_fix","prompt":"case-%d",
          "repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}%s}`, i, i, extra))
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func delivered(cost float64) *Execution {
	r := run(task.StatusDelivered, nil)
	r.Metrics.CostUSD = cost
	return &Execution{Task: &task.Task{ID: "tsk"}, Runs: []*task.Run{r}, Final: r, ChangedFiles: []string{"main.go"}, DurationMS: 1000}
}

func TestRunnerScoresEveryCase(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 3, nil)
	ex := &fakeExecutor{byPrompt: map[string]*Execution{
		"case-0": delivered(0.1), "case-1": delivered(0.1),
		"case-2": {Final: run(task.StatusDead, nil), Runs: []*task.Run{run(task.StatusDead, nil)}},
	}}
	r := &Runner{Suite: s, Executor: ex, WorkRoot: t.TempDir(), Logger: quiet()}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Scored != 3 || rep.Totals.Passed != 2 || rep.Totals.Failed != 1 {
		t.Fatalf("%+v", rep.Totals)
	}
	if len(ex.calls) != 3 {
		t.Errorf("executor called %d times", len(ex.calls))
	}
}

func TestRunnerStopsOnCostCap(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 4, nil)
	ex := &fakeExecutor{byPrompt: map[string]*Execution{
		"case-0": delivered(0.6), "case-1": delivered(0.6), "case-2": delivered(0.6), "case-3": delivered(0.6),
	}}
	r := &Runner{Suite: s, Executor: ex, WorkRoot: t.TempDir(), MaxCostUSD: 1.0, Logger: quiet()}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != 2 {
		t.Fatalf("ran %d cases, want 2 before the $1.00 cap", len(ex.calls))
	}
	if !rep.BudgetExhausted {
		t.Error("report does not record that the cap stopped the suite")
	}
	if rep.Totals.Skipped != 2 {
		t.Errorf("skipped %d", rep.Totals.Skipped)
	}
	// A truncated suite must never be usable as a gate.
	if g := Gate(nil, rep, DefaultLimits()); g.OK {
		t.Error("the gate accepted a report that stopped on its cost cap")
	}
}

func TestRunnerSelectAndTagFilters(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 3, func(i int) string {
		if i == 1 {
			return `,"tags":["cheap"]`
		}
		return ""
	})
	byID := &Runner{Suite: s, Executor: &fakeExecutor{byPrompt: map[string]*Execution{"case-2": delivered(0.1)}},
		WorkRoot: t.TempDir(), Select: []string{"case-2"}, Logger: quiet()}
	rep, err := byID.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Scored != 1 || rep.Totals.Skipped != 2 {
		t.Fatalf("select: %+v", rep.Totals)
	}
	byTag := &Runner{Suite: s, Executor: &fakeExecutor{byPrompt: map[string]*Execution{"case-1": delivered(0.1)}},
		WorkRoot: t.TempDir(), Tags: []string{"cheap"}, Logger: quiet()}
	rep, err = byTag.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Scored != 1 || rep.Totals.Passed != 1 {
		t.Fatalf("tag: %+v", rep.Totals)
	}
}

func TestRunnerSkipIsReportedNotHidden(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 1, func(int) string { return `,"skip":"fixture not captured yet"` })
	r := &Runner{Suite: s, Executor: &fakeExecutor{}, WorkRoot: t.TempDir()}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cases) != 1 || !rep.Cases[0].Skipped || rep.Cases[0].SkipReason != "fixture not captured yet" {
		t.Fatalf("%+v", rep.Cases)
	}
	if rep.Totals.Scored != 0 {
		t.Errorf("a skipped case was scored")
	}
}

func TestRunnerExecutorErrorIsACaseFailure(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 2, nil)
	r := &Runner{Suite: s, Executor: &fakeExecutor{fail: errors.New("docker daemon is not running")}, WorkRoot: t.TempDir()}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err) // one broken case must not abort the suite
	}
	if rep.Totals.Failed != 2 || rep.Totals.Errors != 2 {
		t.Fatalf("%+v", rep.Totals)
	}
	if !strings.Contains(rep.Cases[0].Error, "docker daemon") {
		t.Errorf("error not surfaced: %q", rep.Cases[0].Error)
	}
}

func TestRunnerNeedsAnExecutor(t *testing.T) {
	if _, err := (&Runner{}).Run(context.Background()); err == nil {
		t.Fatal("ran without an executor")
	}
}

func TestRunnerCleansUpFixtures(t *testing.T) {
	requireGit(t)
	s := runnerSuite(t, 1, nil)
	work := t.TempDir()
	r := &Runner{Suite: s, Executor: &fakeExecutor{byPrompt: map[string]*Execution{"case-0": delivered(0.1)}}, WorkRoot: work, Logger: quiet()}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("materialised fixtures left behind: %v", left)
	}
}

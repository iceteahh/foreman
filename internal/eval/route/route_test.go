package route

import (
	"strings"
	"testing"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/judge"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

func report(failed bool) checks.Report {
	r := checks.Report{Results: []checks.Result{{Name: "exit_code", Status: checks.Pass, Evidence: "ok"}}}
	if failed {
		r.Results = append(r.Results, checks.Result{Name: "acceptance_commands", Status: checks.Fail, Evidence: "$ go test\nFAIL"})
	}
	return r
}

func verdict(v string, min int, gamed bool) *judge.Verdict {
	if v == "" {
		return nil
	}
	return &judge.Verdict{Verdict: v, GamedChecks: gamed, Scores: map[string]int{"task_completion": min, "code_quality": 9}, Reasoning: "why"}
}

func TestDecideExhaustive(t *testing.T) {
	type row struct {
		checksFailed bool
		verdict      string // "" = judge skipped
		gamed        bool
		attempt      int
		maxRetries   int
		threshold    int
		minScore     int
		want         Kind
	}
	var rows []row
	for _, cf := range []bool{false, true} {
		for _, v := range []string{"", "pass", "fail", "uncertain"} {
			for _, g := range []bool{false, true} {
				if v == "" && g {
					continue
				}
				for _, att := range []int{1, 2, 3} {
					for _, mr := range []int{0, 2} {
						for _, th := range []int{0, 7} {
							for _, ms := range []int{5, 9} {
								failed := cf || v == "fail" || g
								var want Kind
								switch {
								case failed && att <= mr:
									want = Retry
								case failed:
									want = DeadLetter
								case v == "uncertain":
									want = HumanReview
								case v != "" && ms < th:
									want = HumanReview
								default:
									want = Deliver
								}
								rows = append(rows, row{cf, v, g, att, mr, th, ms, want})
							}
						}
					}
				}
			}
		}
	}
	for _, r := range rows {
		tk := &task.Task{Kind: task.KindCodeFix, Policy: task.Policy{MaxRetries: r.maxRetries, Judge: task.JudgePolicy{Enabled: r.verdict != "", Threshold: r.threshold}}}
		run := &task.Run{Attempt: r.attempt}
		got := Decide(Input{Task: tk, Run: run, Checks: report(r.checksFailed), Judge: verdict(r.verdict, r.minScore, r.gamed), DefaultThreshold: 0})
		if got.Kind != r.want {
			t.Errorf("%+v: got %s (%s) want %s", r, got.Kind, got.Reason, r.want)
		}
		if got.Kind == Retry && (got.Feedback == "" || !strings.Contains(got.Feedback, "Attempt "+itoa(r.attempt))) {
			t.Errorf("%+v: retry without feedback", r)
		}
		if got.Kind != Retry && got.Feedback != "" {
			t.Errorf("%+v: feedback on %s", r, got.Kind)
		}
	}
	if len(rows) < 300 {
		t.Fatalf("only %d rows", len(rows))
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestDecideDetails(t *testing.T) {
	tk := &task.Task{Kind: task.KindCodeFix, Policy: task.Policy{MaxRetries: 2, Judge: task.JudgePolicy{Enabled: true}}}
	// Default threshold applies when the policy has none.
	d := Decide(Input{Task: tk, Run: &task.Run{Attempt: 1}, Checks: report(false), Judge: verdict("pass", 6, false), DefaultThreshold: 7})
	if d.Kind != HumanReview || !strings.Contains(d.Reason, "min score 6 below threshold 7") {
		t.Errorf("%+v", d)
	}
	tk.Policy.Judge.Threshold = 5
	if d := Decide(Input{Task: tk, Run: &task.Run{Attempt: 1}, Checks: report(false), Judge: verdict("pass", 6, false), DefaultThreshold: 7}); d.Kind != Deliver || !strings.Contains(d.Reason, "judge pass (min score 6)") {
		t.Errorf("%+v", d)
	}
	// Reasons name every failing layer; feedback carries evidence and judge reasoning.
	d = Decide(Input{Task: tk, Run: &task.Run{Attempt: 1}, Checks: report(true), Judge: verdict("fail", 2, true)})
	if d.Kind != Retry || !strings.Contains(d.Reason, "checks failed: acceptance_commands") || !strings.Contains(d.Reason, "gamed") {
		t.Errorf("%+v", d)
	}
	for _, want := range []string{"### acceptance_commands", "$ go test\nFAIL", "Independent review (verdict: fail, checks were gamed)", "why", "task_completion=2"} {
		if !strings.Contains(d.Feedback, want) {
			t.Errorf("feedback missing %q", want)
		}
	}
	d = Decide(Input{Task: tk, Run: &task.Run{Attempt: 3}, Checks: report(true)})
	if d.Kind != DeadLetter || !strings.Contains(d.Reason, "retries exhausted (attempt 3, max_retries 2)") {
		t.Errorf("%+v", d)
	}
	// Judge uncertain with an error explains itself.
	u := judge.Uncertain("spawn: no such file")
	if d := Decide(Input{Task: tk, Run: &task.Run{Attempt: 1}, Checks: report(false), Judge: u}); d.Kind != HumanReview || !strings.Contains(d.Reason, "judge uncertain: spawn: no such file") {
		t.Errorf("%+v", d)
	}
	// Review phases go to a human even when everything passes; other phases deliver.
	planned := &task.Task{Kind: task.KindCodeFixPlanned, Policy: tk.Policy, Phases: []task.PhaseSpec{{Name: "plan", Review: true}, {Name: "implement"}}}
	if d := Decide(Input{Task: planned, Run: &task.Run{Attempt: 1, Phase: 1}, Checks: report(false)}); d.Kind != HumanReview || !strings.Contains(d.Reason, "phase 1 (plan) requires approval") {
		t.Errorf("%+v", d)
	}
	if d := Decide(Input{Task: planned, Run: &task.Run{Attempt: 1, Phase: 2}, Checks: report(false)}); d.Kind != Deliver {
		t.Errorf("%+v", d)
	}
	// A failing review phase retries before anyone reviews it.
	if d := Decide(Input{Task: planned, Run: &task.Run{Attempt: 1, Phase: 1}, Checks: report(true)}); d.Kind != Retry {
		t.Errorf("%+v", d)
	}
	if d := Decide(Input{}); d.Kind != DeadLetter {
		t.Errorf("%+v", d)
	}
}

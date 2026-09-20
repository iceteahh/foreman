package feedback

import (
	"strings"
	"testing"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/eval/judge"
)

func TestRender(t *testing.T) {
	rep := checks.Report{Results: []checks.Result{
		{Name: "exit_code", Status: checks.Pass, Evidence: "fine"},
		{Name: "acceptance_commands", Status: checks.Fail, Evidence: "$ npm test\nFAIL src/a.test.js"},
		{Name: "diff_scope", Status: checks.Fail, Evidence: "out-of-scope: README.md"},
		{Name: "schema_validation", Status: checks.NA, Evidence: "no schema"},
	}}
	jv := &judge.Verdict{Verdict: "fail", Scores: map[string]int{"task_completion": 3, "code_quality": 8}, Reasoning: "The fix ignores nil input."}
	out := Render(2, rep, jv, "Please also handle empty strings")
	for _, want := range []string{"Attempt 2 of this task", "## Failed deterministic checks", "### acceptance_commands\n```\n$ npm test\nFAIL src/a.test.js\n```",
		"### diff_scope", "## Independent review (verdict: fail)", "Scores (0–10): code_quality=8, task_completion=3", "The fix ignores nil input.",
		"## Reviewer's note\nPlease also handle empty strings", "do not weaken, skip or delete tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "### exit_code") || strings.Contains(out, "fine") || strings.Contains(out, "schema_validation") {
		t.Error("passing or n/a checks must not appear as failures")
	}
	// Judge pass with nothing failing renders no review section; nil judge is fine.
	passOnly := Render(1, checks.Report{}, &judge.Verdict{Verdict: "pass"}, "")
	if strings.Contains(passOnly, "Independent review") || strings.Contains(passOnly, "Failed deterministic") {
		t.Errorf("%s", passOnly)
	}
	if !strings.Contains(Render(1, rep, nil, ""), "### acceptance_commands") {
		t.Error("nil judge")
	}
	// Uncertain judges are quoted too (human reject path).
	if !strings.Contains(Render(1, checks.Report{}, &judge.Verdict{Verdict: "uncertain", Reasoning: "cannot tell"}, ""), "cannot tell") {
		t.Error("uncertain reasoning dropped")
	}
	// Evidence is clipped.
	big := checks.Report{Results: []checks.Result{{Name: "x", Status: checks.Fail, Evidence: strings.Repeat("y", MaxEvidence+100)}}}
	if got := Render(1, big, nil, ""); !strings.Contains(got, "…(truncated)") || len(got) > MaxEvidence+1000 {
		t.Errorf("not clipped: %d", len(got))
	}
	cold := Cold("Fix the bug in parser.go", "FEEDBACK")
	if !strings.HasPrefix(cold, "Fix the bug in parser.go\n\n---") || !strings.Contains(cold, "no longer available") || !strings.HasSuffix(cold, "FEEDBACK") {
		t.Errorf("%s", cold)
	}
}

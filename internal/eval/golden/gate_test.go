package golden

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// report builds a report from "id:ok" pairs ("a:1 b:0").
func report(spec string) *Report {
	rep := &Report{}
	for _, f := range strings.Fields(spec) {
		id, state, _ := strings.Cut(f, ":")
		switch state {
		case "skip":
			rep.Cases = append(rep.Cases, CaseResult{ID: id, Skipped: true})
		default:
			rep.Cases = append(rep.Cases, CaseResult{ID: id, OK: state == "1", Attempts: 1})
		}
	}
	rep.Summarize()
	return rep
}

func TestGateAcceptsAnUnchangedSuite(t *testing.T) {
	base, cur := report("a:1 b:1 c:0"), report("a:1 b:1 c:0")
	g := Gate(base, cur, DefaultLimits())
	if !g.OK {
		t.Fatalf("reasons %v", g.Reasons)
	}
	if g.Drop != 0 {
		t.Errorf("drop %v", g.Drop)
	}
}

func TestGateBlocksAPassRateDrop(t *testing.T) {
	base := report("a:1 b:1 c:1 d:1 e:1 f:1 g:1 h:1 i:1 j:1") // 100%
	cur := report("a:0 b:0 c:1 d:1 e:1 f:1 g:1 h:1 i:1 j:1")  // 80%
	g := Gate(base, cur, Limits{MaxPassRateDrop: 0.05})
	if g.OK {
		t.Fatal("a 20-point drop passed the gate")
	}
	if g.Drop != 0.2 {
		t.Errorf("drop %v", g.Drop)
	}
	if !strings.Contains(strings.Join(g.Reasons, " "), "pass rate fell") {
		t.Errorf("reasons %v", g.Reasons)
	}
}

func TestGateAllowsADropInsideTheDelta(t *testing.T) {
	base := report("a:1 b:1 c:1 d:1 e:1 f:1 g:1 h:1 i:1 j:1")
	cur := report("a:0 b:1 c:1 d:1 e:1 f:1 g:1 h:1 i:1 j:1") // 90%, 10-point drop
	if g := Gate(base, cur, Limits{MaxPassRateDrop: 0.15, AllowNewFailures: true}); !g.OK {
		t.Fatalf("reasons %v", g.Reasons)
	}
}

// A swapped pass hides a regression behind a stable rate: the gate must catch it.
func TestGateBlocksASwappedPass(t *testing.T) {
	base, cur := report("a:1 b:0"), report("a:0 b:1")
	g := Gate(base, cur, DefaultLimits())
	if g.OK {
		t.Fatal("a swapped pass/fail passed the gate at an unchanged rate")
	}
	if len(g.NewFailures) != 1 || g.NewFailures[0] != "a" {
		t.Errorf("new failures %v", g.NewFailures)
	}
	if len(g.Fixed) != 1 || g.Fixed[0] != "b" {
		t.Errorf("fixed %v", g.Fixed)
	}
	if ok := Gate(base, cur, Limits{MaxPassRateDrop: 0.05, AllowNewFailures: true}); !ok.OK {
		t.Errorf("-allow-new-failures did not lift the block: %v", ok.Reasons)
	}
}

// Deleting a failing case must not be a way to pass the gate.
func TestGateBlocksADisappearingCase(t *testing.T) {
	base, cur := report("a:1 b:0"), report("a:1")
	g := Gate(base, cur, DefaultLimits())
	if g.OK {
		t.Fatal("dropping a failing case passed the gate")
	}
	if len(g.Missing) != 1 || g.Missing[0] != "b" {
		t.Errorf("missing %v", g.Missing)
	}
	if ok := Gate(base, cur, Limits{MaxPassRateDrop: 0.05, AllowMissingCases: true}); !ok.OK {
		t.Errorf("-allow-missing-cases did not lift the block: %v", ok.Reasons)
	}
}

func TestGateReportsNewCases(t *testing.T) {
	base, cur := report("a:1"), report("a:1 b:1")
	g := Gate(base, cur, DefaultLimits())
	if !g.OK {
		t.Fatalf("adding a passing case blocked the gate: %v", g.Reasons)
	}
	if len(g.Added) != 1 || g.Added[0] != "b" {
		t.Errorf("added %v", g.Added)
	}
}

func TestGateIgnoresSkippedCases(t *testing.T) {
	base, cur := report("a:1 b:1"), report("a:1 b:skip")
	if g := Gate(base, cur, DefaultLimits()); !g.OK {
		t.Fatalf("a skipped case counted as a regression: %v", g.Reasons)
	}
}

func TestGateWithoutABaseline(t *testing.T) {
	// First run of the suite: only the absolute floor can apply.
	cur := report("a:1 b:0")
	if g := Gate(nil, cur, DefaultLimits()); !g.OK {
		t.Fatalf("reasons %v", g.Reasons)
	}
	if g := Gate(nil, cur, Limits{MinPassRate: 0.9}); g.OK {
		t.Fatal("a 50% pass rate cleared a 90% floor")
	}
}

func TestGateRejectsAnEmptyRun(t *testing.T) {
	g := Gate(nil, report("a:skip"), DefaultLimits())
	if g.OK || !strings.Contains(strings.Join(g.Reasons, " "), "no case was scored") {
		t.Fatalf("%v", g.Reasons)
	}
}

func TestGateRenderExplainsItself(t *testing.T) {
	g := Gate(report("a:1 b:1"), report("a:0 b:1"), DefaultLimits())
	var sb strings.Builder
	g.Render(&sb)
	out := sb.String()
	for _, want := range []string{"BLOCKED", "newly failing: a", "pass rate"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// ---------- override import ----------

func decision(action review.Action, checksFailed bool, verdict string) review.DecisionRecord {
	return review.DecisionRecord{
		Decision:     review.Decision{RunID: "run_1", Action: action, By: "dana", Comment: "the diff is fine", At: time.Now()},
		TaskID:       "tsk_1",
		JudgeVerdict: verdict,
		ChecksFailed: checksFailed,
	}
}

func TestIsOverride(t *testing.T) {
	for name, tc := range map[string]struct {
		rec  review.DecisionRecord
		want bool
	}{
		"approve despite failing checks": {decision(review.ActionApprove, true, task.VerdictFail), true},
		"approve despite a fail verdict": {decision(review.ActionApprove, false, task.VerdictFail), true},
		"approve after uncertain":        {decision(review.ActionApprove, false, task.VerdictUncertain), false},
		"reject a clean pass":            {decision(review.ActionReject, false, task.VerdictPass), true},
		"reject a failing run":           {decision(review.ActionReject, true, task.VerdictFail), false},
		"reject an uncertain run":        {decision(review.ActionReject, false, task.VerdictUncertain), false},
		"close":                          {decision(review.ActionClose, true, task.VerdictFail), false},
	} {
		if got := IsOverride(tc.rec); got != tc.want {
			t.Errorf("%s: IsOverride = %v, want %v", name, got, tc.want)
		}
	}
}

func TestFromOverrideProducesALoadableSkeleton(t *testing.T) {
	tk := &task.Task{
		ID: "tsk_1", Kind: task.KindCodeFix, Title: "Fix the thing", Prompt: "fix it",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"},
		Policy:    task.Policy{Model: "haiku", MaxTurns: 20, MaxCostUSD: 1, MaxRetries: 2, AllowedTools: []string{"Read"}, TimeoutMS: 1000},
	}
	r := &task.Run{ID: "run_1", TaskID: "tsk_1", Attempt: 2, Status: task.StatusNeedsReview}
	c, err := FromOverride(Override{Decision: decision(review.ActionApprove, true, task.VerdictFail), Task: tk, Run: r})
	if err != nil {
		t.Fatal(err)
	}
	if c.Expect.Outcome != OutcomePass {
		t.Errorf("an approve became %q", c.Expect.Outcome)
	}
	if c.Skip == "" || !strings.Contains(c.Skip, "org/svc") {
		t.Errorf("skip note %q must tell the operator which repo to capture", c.Skip)
	}
	if !strings.Contains(c.Description, "dana") || !strings.Contains(c.Description, "checks failed") {
		t.Errorf("description %q", c.Description)
	}
	if c.Expect.MaxAttempts != 2 {
		t.Errorf("max_attempts %d", c.Expect.MaxAttempts)
	}
	// The skeleton must validate once its fixture exists.
	dir := suiteDir(t)
	c.Repo.Dir = "fixtures/mod"
	if err := WriteCase(dir+"/"+c.ID+".json", c); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCase(dir + "/" + c.ID + ".json")
	if err != nil {
		t.Fatalf("a written skeleton does not load back: %v", err)
	}
	if back.ID != c.ID || back.Prompt != "fix it" {
		t.Errorf("%+v", back)
	}
}

func TestFromOverrideRejectReversesTheExpectation(t *testing.T) {
	tk := &task.Task{ID: "tsk_1", Kind: task.KindCodeFix, Prompt: "p",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"}}
	r := &task.Run{ID: "run_9", Attempt: 1}
	c, err := FromOverride(Override{Decision: decision(review.ActionReject, false, task.VerdictPass), Task: tk, Run: r})
	if err != nil {
		t.Fatal(err)
	}
	if c.Expect.Outcome != OutcomeFail {
		t.Errorf("a reject became %q", c.Expect.Outcome)
	}
}

func TestFromOverrideNeedsTaskAndRun(t *testing.T) {
	if _, err := FromOverride(Override{Decision: decision(review.ActionApprove, true, "")}); err == nil {
		t.Fatal("accepted an override with no task")
	}
}

// An empty model or a zero ceiling in the skeleton would override the kind
// template with nothing, which is worse than not mentioning it at all.
func TestFromOverrideOmitsUnsetBounds(t *testing.T) {
	tk := &task.Task{ID: "tsk_1", Kind: task.KindCodeFix, Prompt: "p",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"},
		Policy:    task.Policy{MaxRetries: 2}} // no model, no turn or cost ceiling
	c, err := FromOverride(Override{Decision: decision(review.ActionReject, false, task.VerdictPass),
		Task: tk, Run: &task.Run{ID: "run_1", Attempt: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(c.Policy, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "max_turns", "max_cost_usd"} {
		if _, ok := got[key]; ok {
			t.Errorf("unset %q was pinned as %v", key, got[key])
		}
	}
	if got["max_retries"] != float64(2) {
		t.Errorf("max_retries %v", got["max_retries"])
	}
	// The override must still merge cleanly onto a kind template.
	base := task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 30, TimeoutMS: 1000, MaxCostUSD: 2.5, MaxRetries: 1}
	merged, err := base.Merge(c.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if merged.MaxTurns != 30 || merged.MaxCostUSD != 2.5 || merged.MaxRetries != 2 {
		t.Errorf("merged %+v", merged)
	}
}

package golden

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/100xteam-ai/foreman/internal/review"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Override is a human review decision that contradicted the machine, together
// with the task and run it decided. Design §5.4: "every human override in the
// review queue (approve-despite-fail or reject-despite-pass) is logged and
// periodically converted into new golden cases".
type Override struct {
	Decision review.DecisionRecord
	Task     *task.Task
	Run      *task.Run
}

// IsOverride reports whether a decision contradicted the machine's own
// reading of the run:
//
//	approve while checks failed or the judge failed → the machine was too strict
//	reject while everything passed                  → the machine was too lax
//
// A decision on an `uncertain` judge is not an override: uncertain means the
// machine asked, and being asked is the system working.
//
// Only the reject branch is reachable through today's router: a Layer 1
// failure or a `fail` verdict routes to retry or dead letter (route.Decide),
// never to a human, so a reviewer is never shown a run the machine rejected.
// The approve branch is kept because it is the design's other half (§5.4) and
// becomes reachable the moment a failing run can be escalated instead of
// dead-lettered — and because a dead letter requeued by hand is the same
// judgement made through a different door.
func IsOverride(rec review.DecisionRecord) bool {
	machineSaidNo := rec.ChecksFailed || rec.JudgeVerdict == task.VerdictFail
	switch rec.Decision.Action {
	case review.ActionApprove:
		return machineSaidNo
	case review.ActionReject:
		return !machineSaidNo && rec.JudgeVerdict == task.VerdictPass
	default:
		return false
	}
}

// FromOverride renders a golden case skeleton pinning the human's answer.
//
// The fixture repository cannot be reconstructed from the decision: the
// workspace is long gone and the upstream ref has moved on. The skeleton
// therefore carries the repo and ref the task used, a `repo.dir` pointing at
// a fixture the operator still has to capture, and a `skip` note saying so —
// an unfinished case that announces itself, rather than one that silently
// scores as a regression.
func FromOverride(o Override) (*Case, error) {
	if o.Task == nil || o.Run == nil {
		return nil, fmt.Errorf("override for run %s: task and run are required", o.Decision.Decision.RunID)
	}
	id := "override-" + strings.TrimPrefix(o.Run.ID, "run_")
	expect := OutcomePass
	if o.Decision.Decision.Action == review.ActionReject {
		expect = OutcomeFail
	}
	// Pin only the bounds that were actually set: an empty "model" or a zero
	// ceiling in the skeleton would override the kind template with nothing,
	// which is worse than not mentioning it.
	et := task.Effective(o.Task, o.Run)
	bounds := map[string]any{"max_retries": et.Policy.MaxRetries}
	if et.Policy.Model != "" {
		bounds["model"] = et.Policy.Model
	}
	if et.Policy.MaxTurns > 0 {
		bounds["max_turns"] = et.Policy.MaxTurns
	}
	if et.Policy.MaxCostUSD > 0 {
		bounds["max_cost_usd"] = et.Policy.MaxCostUSD
	}
	policy, err := json.Marshal(bounds)
	if err != nil {
		return nil, err
	}
	c := &Case{
		ID:          id,
		Description: describeOverride(o),
		Kind:        o.Task.Kind,
		Title:       o.Task.Title,
		Prompt:      et.Prompt,
		Policy:      policy,
		Repo:        Repo{Dir: "fixtures/" + id, Ref: o.Task.Workspace.Ref},
		Expect:      Expect{Outcome: expect, MaxAttempts: o.Run.Attempt},
		Tags:        []string{"override", string(o.Task.Kind)},
		Skip: fmt.Sprintf("capture the fixture repo into evals/golden/fixtures/%s (from %s@%s), then remove this skip",
			id, o.Task.Workspace.Repo, o.Task.Workspace.Ref),
	}
	return c, nil
}

func describeOverride(o Override) string {
	d := o.Decision
	machine := "checks and judge passed"
	switch {
	case d.ChecksFailed && d.JudgeVerdict == task.VerdictFail:
		machine = "checks failed and the judge failed it"
	case d.ChecksFailed:
		machine = "checks failed"
	case d.JudgeVerdict != "":
		machine = "the judge said " + d.JudgeVerdict
	}
	note := strings.TrimSpace(d.Decision.Comment)
	if note != "" {
		note = ": " + firstLine(note)
	}
	return fmt.Sprintf("human override on run %s — %s, but %s chose to %s%s",
		d.Decision.RunID, machine, d.Decision.By, d.Decision.Action, note)
}

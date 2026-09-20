// Package route is Layer 3 (design §5.3): a pure function from the task,
// the run, the Layer 1 report and the Layer 2 verdict to one of four
// decisions. The orchestrator applies the decision; nothing here touches
// storage or spawns processes.
package route

import (
	"fmt"
	"strings"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/eval/feedback"
	"github.com/100xteam-ai/foreman/internal/eval/judge"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Kind is the decision.
type Kind string

const (
	Deliver     Kind = "deliver"
	Retry       Kind = "retry"        // failed → new attempt resuming the session
	DeadLetter  Kind = "dead_letter"  // failed → dead
	HumanReview Kind = "human_review" // evaluating → needs_review
)

// Decision is the router's output.
type Decision struct {
	Kind   Kind
	Reason string
	// Feedback is the retry prompt (Retry only).
	Feedback string
}

// Input is everything the router looks at. Task must be the effective task
// for the run (phase policy applied). Judge is nil when Layer 2 was skipped
// (disabled by policy or short-circuited by a Layer 1 failure).
type Input struct {
	Task   *task.Task
	Run    *task.Run
	Checks checks.Report
	Judge  *judge.Verdict
	// DefaultThreshold is used when policy.judge.threshold is 0 (harness.yaml judge.default_threshold).
	DefaultThreshold int
}

// Threshold is the effective minimum judge score.
func (in Input) Threshold() int {
	if in.Task != nil && in.Task.Policy.Judge.Threshold > 0 {
		return in.Task.Policy.Judge.Threshold
	}
	return in.DefaultThreshold
}

// Decide implements §5.3:
//
//	checks failed or judge fail/gamed → Retry while attempts remain, else DeadLetter
//	phase requires human approval      → HumanReview
//	judge uncertain or min score < threshold → HumanReview
//	otherwise                          → Deliver
//
// "Attempts remain" is run.attempt <= policy.max_retries: max_retries counts
// retries after the first attempt (a policy of 2 allows three runs).
func Decide(in Input) Decision {
	if in.Task == nil || in.Run == nil {
		return Decision{Kind: DeadLetter, Reason: "router called without task or run"}
	}
	var reasons []string
	if in.Checks.Failed() {
		reasons = append(reasons, "checks failed: "+strings.Join(in.Checks.FailedNames(), ", "))
	}
	if in.Judge != nil {
		if in.Judge.GamedChecks {
			reasons = append(reasons, "judge: checks were gamed")
		} else if in.Judge.Verdict == task.VerdictFail {
			reasons = append(reasons, "judge: fail")
		}
	}
	if len(reasons) > 0 {
		reason := strings.Join(reasons, " · ")
		if in.Run.Attempt <= in.Task.Policy.MaxRetries {
			return Decision{Kind: Retry, Reason: reason, Feedback: feedback.Render(in.Run.Attempt, in.Checks, in.Judge, "")}
		}
		return Decision{Kind: DeadLetter, Reason: fmt.Sprintf("%s · retries exhausted (attempt %d, max_retries %d)", reason, in.Run.Attempt, in.Task.Policy.MaxRetries)}
	}
	if ph := in.Task.PhaseSpec(in.Run.Phase); ph != nil && ph.Review {
		return Decision{Kind: HumanReview, Reason: fmt.Sprintf("phase %d (%s) requires approval", in.Run.Phase, ph.Name)}
	}
	if in.Judge != nil {
		if in.Judge.Verdict == task.VerdictUncertain {
			r := "judge uncertain"
			if in.Judge.Error != "" {
				r += ": " + in.Judge.Error
			}
			return Decision{Kind: HumanReview, Reason: r}
		}
		if min, ok := in.Judge.MinScore(); ok && min < in.Threshold() {
			return Decision{Kind: HumanReview, Reason: fmt.Sprintf("judge min score %d below threshold %d", min, in.Threshold())}
		}
	}
	return Decision{Kind: Deliver, Reason: "checks passed" + judgeNote(in.Judge)}
}

func judgeNote(v *judge.Verdict) string {
	if v == nil {
		return " · judge skipped"
	}
	if min, ok := v.MinScore(); ok {
		return fmt.Sprintf(" · judge pass (min score %d)", min)
	}
	return " · judge pass"
}

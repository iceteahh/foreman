package task

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PhaseTemplate is one entry of templates/<kind>/phases/<n>/phase.json plus
// that directory's prompt.md.tmpl. Policy and Acceptance are partial overrides
// on top of the kind defaults.
type PhaseTemplate struct {
	Name string `json:"name"`
	// Review requires a human approval (review queue) before the next phase
	// runs; the phase's output is what gets reviewed.
	Review bool `json:"review"`
	// FanOut marks the planner phase: its structured output is a subtask list,
	// and passing it creates one child task per subtask instead of advancing
	// straight to the next phase (design §6, plan Step 21).
	FanOut     bool            `json:"fan_out,omitempty"`
	Policy     json.RawMessage `json:"policy,omitempty"`
	Acceptance json.RawMessage `json:"acceptance,omitempty"`
	// Prompt is the raw text/template for phases after the first, rendered by
	// the orchestrator when the phase starts (.Plan, .Comment, .Task).
	Prompt string `json:"prompt,omitempty"`
}

// PhaseSpec is a materialised phase stored on the task.
type PhaseSpec struct {
	Name   string `json:"name"`
	Review bool   `json:"review"`
	// FanOut marks the planner phase (see PhaseTemplate.FanOut).
	FanOut     bool       `json:"fan_out,omitempty"`
	Policy     Policy     `json:"policy"`
	Acceptance Acceptance `json:"acceptance"`
	Prompt     string     `json:"prompt,omitempty"`
}

// BuildPhases merges each template on top of the task-level policy and acceptance.
func BuildPhases(base Policy, baseAcceptance Acceptance, phases []PhaseTemplate) ([]PhaseSpec, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	out := make([]PhaseSpec, 0, len(phases))
	for i, ph := range phases {
		p, err := base.Merge(ph.Policy)
		if err != nil {
			return nil, fmt.Errorf("phase %d (%s): %w", i+1, ph.Name, err)
		}
		a, err := baseAcceptance.Merge(ph.Acceptance)
		if err != nil {
			return nil, fmt.Errorf("phase %d (%s): %w", i+1, ph.Name, err)
		}
		name := strings.TrimSpace(ph.Name)
		if name == "" {
			name = fmt.Sprintf("phase-%d", i+1)
		}
		out = append(out, PhaseSpec{Name: name, Review: ph.Review, FanOut: ph.FanOut, Policy: p, Acceptance: a, Prompt: ph.Prompt})
	}
	return out, nil
}

// HasPhases reports whether the task is multi-phase.
func (t *Task) HasPhases() bool { return len(t.Phases) > 0 }

// PhaseSpec returns phase n (1-based) or nil.
func (t *Task) PhaseSpec(n int) *PhaseSpec {
	if n < 1 || n > len(t.Phases) {
		return nil
	}
	return &t.Phases[n-1]
}

// Effective returns the task as the runner, checks, judge and router must see
// it for run r: the phase's policy and acceptance replace the task-level ones,
// and the run's prompt (feedback or phase prompt) replaces task.Prompt. The
// returned copy shares no mutable state with t and must not be stored.
func Effective(t *Task, r *Run) *Task {
	et := *t
	if ph := t.PhaseSpec(r.Phase); ph != nil {
		et.Policy = ph.Policy
		et.Acceptance = ph.Acceptance
	}
	if r.Prompt != "" {
		et.Prompt = r.Prompt
	}
	return &et
}

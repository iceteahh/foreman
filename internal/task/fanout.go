package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ChildTemplate is templates/<kind>/child/task.json plus that directory's
// prompt.md.tmpl: how every fan-out child of the kind is built (plan Step 21).
// Policy and Acceptance are partial overrides on top of the *child kind's*
// defaults, not the parent's — a child is an ordinary task of another kind.
type ChildTemplate struct {
	// Kind is the task kind each child is submitted as (default code_fix).
	Kind Kind `json:"kind"`
	// SessionMode is "fork" (children inherit the planner's reasoning through
	// --resume <planner> --fork-session) or "new" (children start cold).
	SessionMode SessionMode     `json:"session_mode"`
	Policy      json.RawMessage `json:"policy,omitempty"`
	Acceptance  json.RawMessage `json:"acceptance,omitempty"`
	// Prompt is the child prompt template, rendered per subtask when the
	// planner's output is decomposed.
	Prompt string `json:"prompt,omitempty"`
	// MaxChildren caps how many subtasks are turned into children; the rest
	// are reported as dropped rather than silently run (0 = the harness default).
	MaxChildren int `json:"max_children,omitempty"`
}

// ChildSpec is a materialised ChildTemplate stored on the parent task, so a
// fan-out replays from the record even if the template changed since.
type ChildSpec struct {
	Kind        Kind            `json:"kind"`
	SessionMode SessionMode     `json:"session_mode"`
	Policy      json.RawMessage `json:"policy,omitempty"`
	Acceptance  json.RawMessage `json:"acceptance,omitempty"`
	Prompt      string          `json:"prompt,omitempty"`
	MaxChildren int             `json:"max_children,omitempty"`
}

// DefaultMaxChildren bounds one fan-out when neither the template nor the
// config says otherwise. A planner that asks for hundreds of workers is a
// runaway, not a decomposition.
const DefaultMaxChildren = 8

// BuildChild materialises a ChildTemplate, filling the defaults.
func BuildChild(ct *ChildTemplate) (*ChildSpec, error) {
	if ct == nil {
		return nil, nil
	}
	cs := &ChildSpec{Kind: ct.Kind, SessionMode: ct.SessionMode, Prompt: ct.Prompt, MaxChildren: ct.MaxChildren}
	cs.Policy = append(json.RawMessage(nil), ct.Policy...)
	cs.Acceptance = append(json.RawMessage(nil), ct.Acceptance...)
	if cs.Kind == "" {
		cs.Kind = KindCodeFix
	}
	if cs.SessionMode == "" {
		cs.SessionMode = SessionFork
	}
	if cs.MaxChildren <= 0 {
		cs.MaxChildren = DefaultMaxChildren
	}
	return cs, cs.Validate()
}

// Validate checks the child template can produce a submittable task.
func (c *ChildSpec) Validate() error {
	var errs []error
	if !c.Kind.Valid() {
		errs = append(errs, fmt.Errorf("child.kind %q unknown", c.Kind))
	}
	// A child either starts cold or forks the planner. `continue` would make
	// N workers share one transcript, which the CLI cannot do concurrently.
	if c.SessionMode != SessionFork && c.SessionMode != SessionNew {
		errs = append(errs, fmt.Errorf("child.session_mode %q must be fork or new", c.SessionMode))
	}
	if strings.TrimSpace(c.Prompt) == "" {
		errs = append(errs, errors.New("child.prompt is empty"))
	}
	if c.MaxChildren <= 0 {
		errs = append(errs, errors.New("child.max_children must be > 0"))
	}
	for name, raw := range map[string]json.RawMessage{"child.policy": c.Policy, "child.acceptance": c.Acceptance} {
		if len(raw) > 0 && !json.Valid(raw) {
			errs = append(errs, fmt.Errorf("%s is not valid JSON", name))
		}
	}
	return errors.Join(errs...)
}

// FanOutPhase returns the 1-based phase whose output decomposes into children,
// or 0 when the task never fans out.
func (t *Task) FanOutPhase() int {
	for i := range t.Phases {
		if t.Phases[i].FanOut {
			return i + 1
		}
	}
	return 0
}

// FansOut reports whether the task has both a planner phase and a child template.
func (t *Task) FansOut() bool { return t.FanOutPhase() > 0 && t.Child != nil }

// IsChild reports whether the task was created by a parent's fan-out.
func (t *Task) IsChild() bool { return t.ParentID != "" }

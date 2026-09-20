package task

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Spec is the request shape for creating a task (POST /tasks, cron entries,
// `harness run-once`). Policy and Acceptance are partial overrides applied on
// top of the kind's template defaults.
type Spec struct {
	Kind        Kind            `json:"kind"`
	Title       string          `json:"title,omitempty"`
	Prompt      string          `json:"prompt"`
	Workspace   WorkspaceSpec   `json:"workspace"`
	Policy      json.RawMessage `json:"policy,omitempty"`
	Acceptance  json.RawMessage `json:"acceptance,omitempty"`
	Priority    int             `json:"priority"`
	RequestedBy string          `json:"requested_by"`
	// ParentID links a fan-out child to the task whose planner created it
	// (plan Step 21). Set by the orchestrator, never by an API client.
	ParentID string `json:"-"`
}

// Build materialises a Task from the spec and the kind's defaults. phases is
// the kind's phase list (nil for single-phase kinds); every phase policy and
// acceptance is the kind default, then the phase override, then the spec's
// per-task override.
func (s Spec) Build(defaults Policy, defaultAcceptance Acceptance, phases []PhaseTemplate, child *ChildTemplate, now time.Time) (*Task, error) {
	policy, err := defaults.Merge(s.Policy)
	if err != nil {
		return nil, err
	}
	acceptance, err := defaultAcceptance.Merge(s.Acceptance)
	if err != nil {
		return nil, err
	}
	specs, err := BuildPhases(policy, acceptance, phases)
	if err != nil {
		return nil, err
	}
	childSpec, err := BuildChild(child)
	if err != nil {
		return nil, err
	}
	ws := s.Workspace
	if ws.Type == "" {
		ws.Type = "git"
	}
	t := &Task{
		ID:          NewTaskID(),
		Kind:        s.Kind,
		Title:       strings.TrimSpace(s.Title),
		Prompt:      s.Prompt,
		Workspace:   ws,
		Policy:      policy,
		Acceptance:  acceptance,
		Priority:    s.Priority,
		RequestedBy: s.RequestedBy,
		Phases:      specs,
		Child:       childSpec,
		ParentID:    s.ParentID,
		CreatedAt:   now.UTC(),
	}
	if len(specs) > 0 {
		t.Phase = 1
	}
	if t.RequestedBy == "" {
		t.RequestedBy = "api"
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("invalid task: %w", err)
	}
	return t, nil
}

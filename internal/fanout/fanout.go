// Package fanout turns a planner run's structured output into child task
// specs, and the children's outcomes back into the synthesizer's input
// (design §6 fan-out, plan Step 21). Everything here is a pure function: the
// orchestrator moves the data, and no LLM is called from this package.
package fanout

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/templates"
)

// Subtask is one unit of the planner's decomposition. The schema lives in
// templates/<kind>/phases/<n>/phase.json; this is its Go shape.
type Subtask struct {
	Title string `json:"title"`
	// Detail is what the child worker is asked to do.
	Detail string `json:"detail"`
	// Files is the child's diff scope: the only paths it may touch. Two
	// children that claim the same file would race in separate workspaces and
	// deliver conflicting branches, so Validate rejects an overlap.
	Files []string `json:"files"`
	// Commands overrides the child's acceptance commands (empty = the child
	// template's).
	Commands []string `json:"commands,omitempty"`
}

// Plan is the planner phase's structured output.
type Plan struct {
	Summary  string    `json:"summary"`
	Subtasks []Subtask `json:"subtasks"`
	Risks    []string  `json:"risks,omitempty"`
}

// ErrNoSubtasks is returned when the planner produced no decomposition. It is
// a routing signal, not a crash: the parent goes to human review rather than
// silently finishing with nothing done.
var ErrNoSubtasks = errors.New("fanout: planner produced no subtasks")

// Decode parses and validates a planner's structured output.
func Decode(raw json.RawMessage) (*Plan, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, ErrNoSubtasks
	}
	var p Plan
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("fanout: decode plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the plan can be turned into runnable children.
func (p *Plan) Validate() error {
	if len(p.Subtasks) == 0 {
		return ErrNoSubtasks
	}
	var errs []error
	claimed := map[string]int{}
	for i, st := range p.Subtasks {
		n := i + 1
		if strings.TrimSpace(st.Title) == "" {
			errs = append(errs, fmt.Errorf("subtask %d has no title", n))
		}
		if strings.TrimSpace(st.Detail) == "" {
			errs = append(errs, fmt.Errorf("subtask %d (%s) has no detail", n, st.Title))
		}
		if len(st.Files) == 0 {
			errs = append(errs, fmt.Errorf("subtask %d (%s) names no files", n, st.Title))
		}
		for _, f := range st.Files {
			f = path.Clean(strings.TrimSpace(f))
			if f == "" || f == "." || strings.HasPrefix(f, "/") || strings.HasPrefix(f, "..") {
				errs = append(errs, fmt.Errorf("subtask %d (%s): file %q is outside the workspace", n, st.Title, f))
				continue
			}
			// Overlapping scopes are the failure mode that makes a fan-out
			// worse than one worker: two children edit the same file in two
			// workspaces and the second branch silently loses the first's work.
			if prev, dup := claimed[f]; dup {
				errs = append(errs, fmt.Errorf("subtask %d (%s) claims %s, already claimed by subtask %d", n, st.Title, f, prev))
				continue
			}
			claimed[f] = n
		}
	}
	return errors.Join(errs...)
}

// Titles lists every subtask title in order.
func (p *Plan) Titles() []string {
	out := make([]string, 0, len(p.Subtasks))
	for _, st := range p.Subtasks {
		out = append(out, strings.TrimSpace(st.Title))
	}
	return out
}

// Specs renders one task.Spec per subtask from the parent's child template.
// The spec carries the parent id, the parent's workspace, and an acceptance
// override pinning the child's diff scope to the files it claimed. Subtasks
// beyond child.max_children are returned as dropped so the caller can report
// them instead of running a runaway fan-out.
func (p *Plan) Specs(parent *task.Task) (specs []task.Spec, dropped []string, err error) {
	if parent.Child == nil {
		return nil, nil, fmt.Errorf("fanout: task %s has no child template", parent.ID)
	}
	if err := parent.Child.Validate(); err != nil {
		return nil, nil, err
	}
	subtasks := p.Subtasks
	if max := parent.Child.MaxChildren; len(subtasks) > max {
		for _, st := range subtasks[max:] {
			dropped = append(dropped, strings.TrimSpace(st.Title))
		}
		subtasks = subtasks[:max]
	}
	// Siblings are the subtasks that actually became children: naming a
	// dropped one would tell a worker to leave files alone that nobody is
	// going to touch.
	titles := make([]string, 0, len(subtasks))
	for _, st := range subtasks {
		titles = append(titles, strings.TrimSpace(st.Title))
	}
	for i, st := range subtasks {
		spec, err := childSpec(parent, p, st, titles, i, len(subtasks))
		if err != nil {
			return nil, nil, err
		}
		specs = append(specs, spec)
	}
	return specs, dropped, nil
}

func childSpec(parent *task.Task, p *Plan, st Subtask, titles []string, i, total int) (task.Spec, error) {
	c := parent.Child
	prompt, err := templates.RenderChild(c.Prompt, templates.ChildData{
		Title: strings.TrimSpace(st.Title), Detail: strings.TrimSpace(st.Detail),
		Files: st.Files, Siblings: without(titles, i), Summary: strings.TrimSpace(p.Summary),
		Task: parent.Prompt, Index: i + 1, Total: total,
	})
	if err != nil {
		return task.Spec{}, err
	}
	acceptance, err := overrideAcceptance(c.Acceptance, st, parent.Acceptance.Commands)
	if err != nil {
		return task.Spec{}, fmt.Errorf("fanout: subtask %d (%s): %w", i+1, st.Title, err)
	}
	return task.Spec{
		Kind:        c.Kind,
		Title:       strings.TrimSpace(st.Title),
		Prompt:      prompt,
		Workspace:   parent.Workspace,
		Policy:      append(json.RawMessage(nil), c.Policy...),
		Acceptance:  acceptance,
		Priority:    parent.Priority,
		RequestedBy: "fanout:" + parent.ID,
		ParentID:    parent.ID,
	}, nil
}

// overrideAcceptance layers the subtask's scope on top of the child template's
// acceptance override. diff_scope is always the subtask's files: the planner
// decided who owns what, and a child that may write outside its slice can
// clobber a sibling's file in its own workspace.
//
// Commands fall back to the parent task's: the repository's test command is a
// property of the repository, and a child that runs no acceptance command is
// judged on a diff nobody compiled. Precedence is subtask, then child
// template, then parent.
func overrideAcceptance(base json.RawMessage, st Subtask, parentCommands []string) (json.RawMessage, error) {
	obj := map[string]json.RawMessage{}
	if s := strings.TrimSpace(string(base)); s != "" && s != "null" {
		if err := json.Unmarshal(base, &obj); err != nil {
			return nil, fmt.Errorf("child.acceptance: %w", err)
		}
	}
	scope, err := json.Marshal(diffScope(st.Files))
	if err != nil {
		return nil, err
	}
	obj["diff_scope"] = scope
	switch {
	case len(st.Commands) > 0:
		cmds, err := json.Marshal(st.Commands)
		if err != nil {
			return nil, err
		}
		obj["commands"] = cmds
	case len(obj["commands"]) > 0:
		// the child template named them
	case len(parentCommands) > 0:
		cmds, err := json.Marshal(parentCommands)
		if err != nil {
			return nil, err
		}
		obj["commands"] = cmds
	}
	return json.Marshal(obj)
}

// diffScope turns the claimed files into checks.DiffScope patterns, sorted so
// the same plan always produces the same spec.
func diffScope(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = path.Clean(strings.TrimSpace(f))
		if f == "" || f == "." || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func without(ss []string, i int) []string {
	out := make([]string, 0, len(ss))
	for j, s := range ss {
		if j != i {
			out = append(out, s)
		}
	}
	return out
}

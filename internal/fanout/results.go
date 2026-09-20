package fanout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/templates"
)

// Child pairs a fan-out child task with its newest run.
type Child struct {
	Task *task.Task
	Run  *task.Run
}

// Done reports whether the child has stopped moving on its own: its newest run
// is terminal (delivered, dead, closed). A child in needs_review is *not*
// done — a human still owes it a decision, and synthesising over a change
// nobody approved would deliver work that was never accepted.
func (c Child) Done() bool { return c.Run != nil && c.Run.Status.Terminal() }

// Delivered reports whether the child produced its artifact.
func (c Child) Delivered() bool { return c.Run != nil && c.Run.Status == task.StatusDelivered }

// Blocked reports whether the child is waiting for a human. It is not done —
// the fan-in must not synthesise over it — but no amount of pool work will
// move it either, so a caller draining the tree should stop and say so.
func (c Child) Blocked() bool { return c.Run != nil && c.Run.Status == task.StatusNeedsReview }

// Settled reports whether the child has stopped needing the pool: finished, or
// waiting for a person.
func (c Child) Settled() bool { return c.Done() || c.Blocked() }

// Pending returns the children that are still moving, newest run first seen.
func Pending(children []Child) []Child {
	var out []Child
	for _, c := range children {
		if !c.Done() {
			out = append(out, c)
		}
	}
	return out
}

// AllDone reports whether every child is terminal. An empty slice is not done:
// a parent with no children never fanned out.
func AllDone(children []Child) bool {
	if len(children) == 0 {
		return false
	}
	return len(Pending(children)) == 0
}

// AllSettled reports whether no child can progress without a human: every one
// is terminal or waiting in review. An empty slice is not settled.
func AllSettled(children []Child) bool {
	if len(children) == 0 {
		return false
	}
	for _, c := range children {
		if !c.Settled() {
			return false
		}
	}
	return true
}

// Blocked returns the children waiting for a human decision.
func Blocked(children []Child) []Child {
	var out []Child
	for _, c := range children {
		if c.Blocked() {
			out = append(out, c)
		}
	}
	return out
}

// Delivered counts the children that delivered.
func Delivered(children []Child) int {
	n := 0
	for _, c := range children {
		if c.Delivered() {
			n++
		}
	}
	return n
}

// Summaries renders the children for the synthesizer prompt. It never includes
// a child's transcript — the synthesizer sees what the harness recorded
// (status, checks, artifacts, structured output), the same evidence a human
// reviewer gets.
func Summaries(children []Child) []templates.ChildSummary {
	out := make([]templates.ChildSummary, 0, len(children))
	for _, c := range children {
		s := templates.ChildSummary{Status: "not started"}
		if c.Task != nil {
			s.Title = c.Task.Title
			s.TaskID = c.Task.ID
			if s.Title == "" {
				s.Title = c.Task.ID
			}
		}
		if c.Run != nil {
			s.RunID = c.Run.ID
			s.Status = string(c.Run.Status)
			s.Artifacts = append([]string(nil), c.Run.Artifacts...)
			s.Error = c.Run.LastError
			s.Checks = checkSummary(c.Run)
			s.Output = pretty(c.Run.Output)
		}
		out = append(out, s)
	}
	return out
}

// checkSummary renders a run's Layer 1 outcomes as "tests=pass lint=fail".
func checkSummary(r *task.Run) string {
	if r.Eval == nil || len(r.Eval.Checks) == 0 {
		return ""
	}
	names := make([]string, 0, len(r.Eval.Checks))
	for n := range r.Eval.Checks {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%s", n, r.Eval.Checks[n].Status))
	}
	return strings.Join(parts, " ")
}

func pretty(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") != nil {
		return s
	}
	return buf.String()
}

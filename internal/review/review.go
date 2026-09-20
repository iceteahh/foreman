// Package review is the human-in-the-loop seam (design §5.3 "human review
// queue", §6): runs that land in needs_review are posted to a Channel, and a
// Decider applies the human's approve / reject / close to the state machine.
// The orchestrator implements Decider; Slack (and the HTTP API) are Channels
// and sources of decisions. Every decision is recorded so plan Step 19 can
// turn overrides into golden cases.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/workspace"
)

// Action is what a reviewer can do with a needs_review run (§3.2 edges).
type Action string

const (
	ActionApprove Action = "approve" // needs_review → delivered (or next phase)
	ActionReject  Action = "reject"  // needs_review → retry with the comment as feedback
	ActionClose   Action = "close"   // needs_review → closed
)

// Valid reports whether a is a known action.
func (a Action) Valid() bool {
	switch a {
	case ActionApprove, ActionReject, ActionClose:
		return true
	}
	return false
}

// Request is everything a reviewer needs to decide. Like the judge, it never
// includes the worker transcript.
type Request struct {
	Task *task.Task
	Run  *task.Run
	// Reason is the router's explanation ("judge uncertain", "phase plan requires approval").
	Reason  string
	Checks  checks.Report
	Judge   *task.JudgeVerdict
	Capture *workspace.Capture
	// Output is the run's structured output (a plan) when it produced one.
	Output json.RawMessage
	// Phase is the phase spec the run executed, nil for single-phase tasks.
	Phase *task.PhaseSpec
	// Escalation is set when the SLA expired (re-post with a mention).
	Escalation bool
}

// Title is the short human label for the request.
func (r *Request) Title() string {
	if r.Task == nil {
		return ""
	}
	if t := strings.TrimSpace(r.Task.Title); t != "" {
		return t
	}
	p := strings.TrimSpace(r.Task.Prompt)
	if i := strings.IndexAny(p, "\r\n"); i >= 0 {
		p = p[:i]
	}
	if len(p) > 80 {
		p = p[:79] + "…"
	}
	return p
}

// Ref identifies a posted message so it can be updated or escalated.
type Ref struct {
	Channel string `json:"channel"`
	ID      string `json:"id"` // Slack message ts
}

// String renders "channel/id".
func (r Ref) String() string { return r.Channel + "/" + r.ID }

// ParseRef inverts String.
func ParseRef(s string) Ref {
	c, id, _ := strings.Cut(s, "/")
	return Ref{Channel: c, ID: id}
}

// Channel posts review requests to humans (Slack, log, e-mail…).
type Channel interface {
	Name() string
	// Post publishes the request and returns where it landed.
	Post(ctx context.Context, req *Request) (Ref, error)
	// Escalate re-posts an unanswered request past its SLA.
	Escalate(ctx context.Context, req *Request, ref Ref) error
	// Resolve updates the original post once a decision was applied.
	Resolve(ctx context.Context, req *Request, ref Ref, d Decision, out *Outcome) error
}

// Decision is a human's verdict on a run.
type Decision struct {
	RunID   string    `json:"run_id"`
	Action  Action    `json:"action"`
	By      string    `json:"by"`
	Comment string    `json:"comment,omitempty"`
	Source  string    `json:"source,omitempty"` // slack | api | cli
	At      time.Time `json:"at"`
}

// Validate checks the decision is well formed.
func (d Decision) Validate() error {
	var errs []error
	if d.RunID == "" {
		errs = append(errs, errors.New("run_id is empty"))
	}
	if !d.Action.Valid() {
		errs = append(errs, fmt.Errorf("action %q unknown (approve|reject|close)", d.Action))
	}
	if strings.TrimSpace(d.By) == "" {
		errs = append(errs, errors.New("by is empty"))
	}
	return errors.Join(errs...)
}

// Outcome is what applying a decision produced.
type Outcome struct {
	Run *task.Run `json:"run"`
	// NextRun is the retry (reject) or next-phase run (approve on a phase).
	NextRun   *task.Run `json:"next_run,omitempty"`
	Artifacts []string  `json:"artifacts,omitempty"`
	Message   string    `json:"message"`
}

// ErrNotReviewable is returned when the run is not in needs_review.
var ErrNotReviewable = errors.New("review: run is not waiting for review")

// Decider applies a decision to the run's state machine. The orchestrator
// implements it; channels call it.
type Decider interface {
	Decide(ctx context.Context, d Decision) (*Outcome, error)
}

// Post is the stored record of a posted request (SLA tracking, Step 12).
type Post struct {
	RunID       string
	TaskID      string
	Channel     string
	Ref         Ref
	PostedAt    time.Time
	EscalatedAt *time.Time
	ResolvedAt  *time.Time
}

// DecisionRecord is one row of review_decisions.
type DecisionRecord struct {
	ID        int64
	Decision  Decision
	TaskID    string
	NextRunID string
	// Context for the golden suite: what the machine said before the human overrode it.
	JudgeVerdict string
	ChecksFailed bool
}

// Store persists posts and decisions (implemented by the SQLite store).
type Store interface {
	SavePost(ctx context.Context, p Post) error
	GetPost(ctx context.Context, runID string) (*Post, error)
	// ListPostsForEscalation returns unresolved, not yet escalated posts older than postedBefore.
	ListPostsForEscalation(ctx context.Context, postedBefore time.Time) ([]Post, error)
	MarkEscalated(ctx context.Context, runID string, at time.Time) error
	MarkResolved(ctx context.Context, runID string, at time.Time) error
	RecordDecision(ctx context.Context, rec DecisionRecord) (int64, error)
	ListDecisions(ctx context.Context, runID string) ([]DecisionRecord, error)
	// ListDecisionsSince returns every decision taken at or after the given
	// time, newest first, capped at limit (0 = no cap). Plan Step 19 turns the
	// overrides among them into golden cases.
	ListDecisionsSince(ctx context.Context, since time.Time, limit int) ([]DecisionRecord, error)
}

// ErrPostNotFound is returned by GetPost for an unknown run.
var ErrPostNotFound = errors.New("review: post not found")

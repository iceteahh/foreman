// Package deadletter makes exhausted runs visible and actionable (plan Step
// 18, design §9): every run that reaches `dead` is recorded with the evidence
// an operator needs, paged once, and requeueable with `harness requeue`.
package deadletter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Entry is one dead-lettered run.
type Entry struct {
	RunID  string `json:"run_id"`
	TaskID string `json:"task_id"`
	Kind   string `json:"kind"`
	// Reason is why it died ("checks failed after 3 attempts", "job attempts exhausted").
	Reason  string `json:"reason"`
	Attempt int    `json:"attempt"`
	Phase   int    `json:"phase,omitempty"`
	// Feedback is the last failure evidence: the check summary and the judge's
	// reasoning, so the page says what actually went wrong.
	Feedback    string     `json:"feedback,omitempty"`
	EventLogURI string     `json:"event_log_uri,omitempty"`
	DeadAt      time.Time  `json:"dead_at"`
	PagedAt     *time.Time `json:"paged_at,omitempty"`
	RequeuedAt  *time.Time `json:"requeued_at,omitempty"`
	// RequeueRunID is the run created by `harness requeue`.
	RequeueRunID string `json:"requeue_run_id,omitempty"`
}

// Open reports whether the entry still needs an operator.
func (e Entry) Open() bool { return e.RequeuedAt == nil }

// ErrNotFound is returned for an unknown run id.
var ErrNotFound = errors.New("deadletter: entry not found")

// Store persists entries (implemented by the SQLite store).
type Store interface {
	SaveEntry(ctx context.Context, e Entry) error
	GetEntry(ctx context.Context, runID string) (*Entry, error)
	// ListOpen returns unrequeued entries, oldest first.
	ListOpen(ctx context.Context, limit int) ([]Entry, error)
	MarkPaged(ctx context.Context, runID string, at time.Time) error
	MarkRequeued(ctx context.Context, runID, newRunID string, at time.Time) error
}

// Pager delivers the alert. It is the same seam budget.Pager uses, so one
// Slack/PagerDuty sink serves budget breaches and dead letters.
type Pager interface {
	Page(ctx context.Context, subject, detail string)
}

// Sink is what the orchestrator calls when a run dies. The interface keeps the
// pool independent of storage and alerting.
type Sink interface {
	Dead(ctx context.Context, e Entry)
}

// Recorder stores the entry and pages once.
type Recorder struct {
	Store  Store
	Pager  Pager
	Logger *slog.Logger
	Now    func() time.Time
	// ReplayHint is printed in the page so the operator knows what to run
	// (default "harness replay <run_id>").
	ReplayHint func(runID string) string
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recorder) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Dead implements Sink: persist, log, page.
func (r *Recorder) Dead(ctx context.Context, e Entry) {
	if r == nil {
		return
	}
	if e.DeadAt.IsZero() {
		e.DeadAt = r.now()
	}
	log := r.log().With("run_id", e.RunID, "task_id", e.TaskID, "kind", e.Kind, "attempt", e.Attempt)
	if r.Store != nil {
		if err := r.Store.SaveEntry(ctx, e); err != nil {
			log.Error("recording the dead letter failed", "err", err)
		}
	}
	log.Error("run dead-lettered", "reason", e.Reason, "event_log", e.EventLogURI)
	if r.Pager == nil {
		return
	}
	r.Pager.Page(ctx, fmt.Sprintf("harness: run %s dead-lettered (%s)", e.RunID, e.Kind), r.Detail(e))
	if r.Store != nil {
		at := r.now()
		if err := r.Store.MarkPaged(ctx, e.RunID, at); err != nil {
			log.Warn("marking the dead letter paged failed", "err", err)
		}
	}
}

// Detail is the human-readable page body.
func (r *Recorder) Detail(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s (%s) exhausted its retries on attempt %d", e.TaskID, e.Kind, e.Attempt)
	if e.Phase > 0 {
		fmt.Fprintf(&b, " of phase %d", e.Phase)
	}
	fmt.Fprintf(&b, ".\nReason: %s\n", e.Reason)
	if e.Feedback != "" {
		fmt.Fprintf(&b, "\nLast failure evidence:\n%s\n", clip(e.Feedback, 1500))
	}
	if e.EventLogURI != "" {
		fmt.Fprintf(&b, "\nEvent log: %s", e.EventLogURI)
	}
	hint := r.ReplayHint
	if hint == nil {
		hint = func(id string) string { return "harness replay " + id }
	}
	fmt.Fprintf(&b, "\nReplay: %s\nRequeue: harness requeue %s", hint(e.RunID), e.RunID)
	return b.String()
}

// EntryFor builds the entry for a run from the stored state, so the page
// carries the same evidence a retry would have received as feedback.
func EntryFor(t *task.Task, r *task.Run, reason string) Entry {
	e := Entry{RunID: r.ID, TaskID: r.TaskID, Kind: string(t.Kind), Reason: reason,
		Attempt: r.Attempt, Phase: r.Phase, EventLogURI: r.EventLogURI}
	var parts []string
	if r.Eval != nil {
		rep := checks.FromOutcomes(r.Eval.Checks)
		if names := rep.FailedNames(); len(names) > 0 {
			parts = append(parts, "failed checks: "+strings.Join(names, ", "))
			for _, name := range names {
				if c, ok := rep.Get(name); ok && c.Evidence != "" {
					parts = append(parts, name+": "+clip(c.Evidence, 400))
				}
			}
		}
		if j := r.Eval.Judge; j != nil {
			line := "judge: " + j.Verdict
			if j.GamedChecks {
				line += " (gamed_checks)"
			}
			if j.Reasoning != "" {
				line += " — " + clip(j.Reasoning, 400)
			}
			parts = append(parts, line)
		}
	}
	if r.LastError != "" {
		parts = append(parts, "last error: "+clip(r.LastError, 400))
	}
	if len(r.Output) > 0 {
		var pretty strings.Builder
		if b, err := json.Marshal(json.RawMessage(r.Output)); err == nil {
			pretty.Write(b)
			parts = append(parts, "output: "+clip(pretty.String(), 300))
		}
	}
	e.Feedback = strings.Join(parts, "\n")
	return e
}

// clip shortens s to n runes, with an ellipsis when it had to cut.
// A byte slice would split a multi-byte rune and emit U+FFFD, so the cut
// counts runes: these strings carry worker output and issue text, which are
// routinely not ASCII.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// LogPager is the default Pager: it writes the alert to the log. Step 18's
// Slack sink is review/slack.Pager; PagerDuty can be added behind this seam.
type LogPager struct {
	Logger *slog.Logger
}

// Page implements Pager and budget.Pager.
func (p LogPager) Page(_ context.Context, subject, detail string) {
	l := p.Logger
	if l == nil {
		l = slog.Default()
	}
	l.Error("PAGE "+subject, "detail", detail)
}

var (
	_ Sink  = (*Recorder)(nil)
	_ Pager = LogPager{}
)

package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// Build assembles a Request from stored state. capture may be nil (after the
// workspace is gone); reason defaults to run.last_error.
func Build(t *task.Task, r *task.Run, capture *workspace.Capture, reason string) *Request {
	req := &Request{Task: t, Run: r, Reason: reason, Capture: capture, Output: r.Output, Phase: t.PhaseSpec(r.Phase)}
	if req.Reason == "" {
		req.Reason = r.LastError
	}
	if r.Eval != nil {
		req.Checks = checks.FromOutcomes(r.Eval.Checks)
		req.Judge = r.Eval.Judge
	}
	return req
}

// Escalator re-posts unanswered requests past the SLA (harness.yaml review.sla_hours).
type Escalator struct {
	Store   store.Store
	Reviews Store
	Channel Channel
	SLA     time.Duration
	// Interval between sweeps (default 5 minutes).
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

func (e *Escalator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Escalator) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Run sweeps until ctx ends.
func (e *Escalator) Run(ctx context.Context) {
	if e.SLA <= 0 || e.Reviews == nil || e.Channel == nil {
		return
	}
	iv := e.Interval
	if iv <= 0 {
		iv = 5 * time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := e.Once(ctx); err != nil {
				e.log().Warn("review escalation sweep failed", "err", err)
			} else if n > 0 {
				e.log().Info("review escalations sent", "count", n)
			}
		}
	}
}

// Once escalates every unresolved post older than SLA and returns how many.
func (e *Escalator) Once(ctx context.Context) (int, error) {
	if e.SLA <= 0 {
		return 0, nil
	}
	now := e.now()
	posts, err := e.Reviews.ListPostsForEscalation(ctx, now.Add(-e.SLA))
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, p := range posts {
		r, err := e.Store.GetRun(ctx, p.RunID)
		if err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", p.RunID, err))
			continue
		}
		if r.Status != task.StatusNeedsReview {
			// Decided out of band; close the post so it stops matching.
			_ = e.Reviews.MarkResolved(ctx, p.RunID, now)
			continue
		}
		t, err := e.Store.GetTask(ctx, r.TaskID)
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", r.TaskID, err))
			continue
		}
		req := Build(t, r, nil, "")
		req.Escalation = true
		if err := e.Channel.Escalate(ctx, req, p.Ref); err != nil {
			errs = append(errs, fmt.Errorf("escalate %s: %w", p.RunID, err))
			continue
		}
		if err := e.Reviews.MarkEscalated(ctx, p.RunID, now); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

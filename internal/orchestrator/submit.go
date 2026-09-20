// Package orchestrator owns the run lifecycle: submission, the worker pool,
// evaluation, routing and delivery. It never calls an LLM itself.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/session"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/templates"
)

// Submitter turns a Spec into a stored task, its first run, and a queue job.
type Submitter struct {
	Store store.Store
	Queue queue.Queue
	// Sessions copies a planner transcript into a fan-out child's namespace so
	// the child can fork it (plan Step 21); required only for SubmitOptions.ForkFrom.
	Sessions session.Store
	Now      func() time.Time
}

// SubmitOptions carry what a fan-out needs beyond the spec. They are set by
// the orchestrator, never by an API client.
type SubmitOptions struct {
	// ForkFrom is the planner session a fan-out child inherits: the first run
	// becomes `--resume <ForkFrom> --fork-session --session-id <new>`. Empty
	// starts the child cold.
	ForkFrom string
	// ForkFromTask owns the snapshot named by ForkFrom (the parent task).
	ForkFromTask string
}

// Submit creates the task and run atomically enough for M1: the task and run
// rows are written first, then the job. A crash between the two leaves a
// queued run with no job; `harness requeue` (Step 18) covers that.
func (s *Submitter) Submit(ctx context.Context, spec task.Spec) (*task.Task, *task.Run, error) {
	return s.SubmitWith(ctx, spec, SubmitOptions{})
}

// SubmitWith is Submit with fan-out options (plan Step 21).
func (s *Submitter) SubmitWith(ctx context.Context, spec task.Spec, opts SubmitOptions) (*task.Task, *task.Run, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	policy, err := templates.Policy(spec.Kind)
	if err != nil {
		return nil, nil, fmt.Errorf("unknown task kind %q: %w", spec.Kind, err)
	}
	acceptance, err := templates.Acceptance(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	phases, err := templates.Phases(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	child, err := templates.Child(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	t, err := spec.Build(policy, acceptance, phases, child, now())
	if err != nil {
		return nil, nil, err
	}
	r, err := s.firstRun(ctx, t, opts, now())
	if err != nil {
		return nil, nil, err
	}
	// The resume pointer is known before anything is spawned (design §4.4).
	// For a fork it is the child's own new id, not the planner's: the child
	// writes its own transcript, and a retry must resume that one.
	t.SessionID = r.SessionID
	if err := s.Store.CreateTask(ctx, t); err != nil {
		return nil, nil, err
	}
	if err := s.Store.CreateRun(ctx, r); err != nil {
		return nil, nil, err
	}
	if _, err := s.Queue.Enqueue(ctx, queue.Job{RunID: r.ID, TaskID: t.ID, Kind: string(t.Kind), Priority: t.Priority}, 0); err != nil {
		return nil, nil, err
	}
	return t, r, nil
}

// firstRun builds the task's first run: a plain `new` session, or a `fork` of
// the planner's when the caller asked for one. The planner snapshot is copied
// into the child's namespace first, because the runner restores a transcript
// by the task id it is running under (design §4.4).
func (s *Submitter) firstRun(ctx context.Context, t *task.Task, opts SubmitOptions, now time.Time) (*task.Run, error) {
	if opts.ForkFrom == "" {
		return task.NewRun(t, now), nil
	}
	if s.Sessions == nil {
		return nil, errors.New("submit: forking a child needs a session store")
	}
	if opts.ForkFromTask == "" {
		return nil, errors.New("submit: fork_from_task is required to locate the planner snapshot")
	}
	if _, err := s.Sessions.Copy(ctx, opts.ForkFromTask, t.ID, opts.ForkFrom); err != nil {
		return nil, fmt.Errorf("submit: copy planner transcript: %w", err)
	}
	return task.ForkRun(t, opts.ForkFrom, now)
}

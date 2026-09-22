// Package store persists tasks and runs. The interface is Postgres-ready: every
// implementation lives in its own sub-package and no SQL leaks out of it.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

// ErrNotFound is returned when a task or run id is unknown.
var ErrNotFound = errors.New("store: not found")

// ErrPhaseChanged is returned by UpdateTaskPhase when the task has left the
// phase the caller believed it was in.
var ErrPhaseChanged = errors.New("store: task is no longer in the expected phase")

// Store is the persistence seam (plan Step 3).
type Store interface {
	CreateTask(ctx context.Context, t *task.Task) error
	GetTask(ctx context.Context, id string) (*task.Task, error)
	// UpdateTaskSession moves the task's resume pointer.
	UpdateTaskSession(ctx context.Context, taskID, sessionID string) error
	// UpdateTaskPhase moves the task from phase `from` into phase `to`. Like
	// AdvancePhase it is a compare-and-swap: a caller holding a stale view of
	// the task (a requeued run from an earlier phase, a replica that lost its
	// lease and came back) would otherwise rewind a task that has already
	// moved on. A task no longer at `from` yields ErrPhaseChanged.
	UpdateTaskPhase(ctx context.Context, taskID string, from, to int) error
	// ListChildTasks returns the fan-out children of a parent in creation
	// order (plan Step 21); an empty slice for a task that never fanned out.
	ListChildTasks(ctx context.Context, parentID string) ([]*task.Task, error)
	// ListFanOutParents returns the ids of tasks that have at least one child,
	// so the fan-in sweeper can recover parents whose synthesizer was never
	// enqueued (a crash between the last child finishing and the enqueue).
	ListFanOutParents(ctx context.Context) ([]string, error)
	// AdvancePhase moves a task from phase `from` to phase `to` and reports
	// whether it won: a compare-and-swap, so N children finishing at once
	// enqueue exactly one synthesizer run.
	AdvancePhase(ctx context.Context, taskID string, from, to int) (bool, error)
	// ListTerminalTasks returns ids of tasks whose newest run is terminal
	// (delivered, dead, closed) and finished before the given time; the
	// session sweeper uses it (retention.sessions_days).
	ListTerminalTasks(ctx context.Context, finishedBefore time.Time) ([]string, error)

	CreateRun(ctx context.Context, r *task.Run) error
	GetRun(ctx context.Context, id string) (*task.Run, error)
	// UpdateRunStatus applies task.Transition at the store boundary; illegal
	// edges return a *task.TransitionError and write nothing. reason is stored
	// as run.last_error when non-empty.
	UpdateRunStatus(ctx context.Context, runID string, to task.RunStatus, reason string) error
	ListRunsByStatus(ctx context.Context, status task.RunStatus) ([]*task.Run, error)
	ListRunsByTask(ctx context.Context, taskID string) ([]*task.Run, error)
	// LatestRun returns the most recently created run of a task.
	LatestRun(ctx context.Context, taskID string) (*task.Run, error)
	// LatestRuns is LatestRun for many tasks in one round trip, keyed by task
	// id. A task with no runs is absent from the map rather than an error. The
	// fan-in asks for every child of a parent, on every sweep, for every
	// parent: one query per child is where that becomes parents × children.
	LatestRuns(ctx context.Context, taskIDs []string) (map[string]*task.Run, error)
	// CountAttempts returns the number of runs recorded for a task.
	CountAttempts(ctx context.Context, taskID string) (int, error)

	RecordWorker(ctx context.Context, runID string, w task.WorkerInfo) error
	RecordMetrics(ctx context.Context, runID string, m task.Metrics) error
	RecordEval(ctx context.Context, runID string, e task.Eval) error
	// RecordOutput stores result.structured_output (a plan) on the run.
	RecordOutput(ctx context.Context, runID string, output json.RawMessage) error
	RecordArtifacts(ctx context.Context, runID string, eventLogURI, sessionURI string, artifacts []string) error

	Close() error
}

// Leases is the singleton-job seam for stateless orchestrator replicas (plan
// Step 22). Periodic work that must not run twice — SLA escalation, the
// session sweep, the fan-in sweep — takes a named lease first. It is
// leader-free: no election, no coordinator, just a row with an expiry that any
// replica may reclaim once it lapses.
type Leases interface {
	// AcquireLease grants name to owner until now+ttl when the lease is free,
	// held by owner already (a renewal), or expired. It reports whether owner
	// holds it afterwards.
	AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	// ReleaseLease drops the lease when owner still holds it; releasing one
	// held by somebody else is a no-op.
	ReleaseLease(ctx context.Context, name, owner string) error
}

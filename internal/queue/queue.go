// Package queue is the durable buffer between intake and the worker pool.
// The interface stays fixed while the implementation moves from SQLite to
// River/SQS (plan Steps 8 and 22).
package queue

import (
	"context"
	"errors"
	"time"
)

// ErrEmpty is returned by Lease when no job is ready.
var ErrEmpty = errors.New("queue: no job available")

// ErrLeaseLost is returned when Ack/Nack/Extend is called with a stale lease.
var ErrLeaseLost = errors.New("queue: lease lost")

// Job is one unit of work: "run this run".
type Job struct {
	ID       int64
	RunID    string
	TaskID   string
	Kind     string
	Priority int
	// Attempts counts leases so far (including the current one).
	Attempts    int
	AvailableAt time.Time
	LeasedUntil time.Time
	LeaseToken  string
	CreatedAt   time.Time
}

// Depth is a queue snapshot for metrics and /healthz.
type Depth struct {
	Ready, Leased, Dead int
	OldestReadyAge      time.Duration
}

// Queue is the seam.
type Queue interface {
	// Enqueue adds a job that becomes available after delay.
	Enqueue(ctx context.Context, j Job, delay time.Duration) (int64, error)
	// Lease atomically takes the highest-priority ready job of one of kinds
	// (nil = any) for leaseFor. Expired leases are reclaimable.
	Lease(ctx context.Context, kinds []string, leaseFor time.Duration) (*Job, error)
	// Extend renews the lease (heartbeat for long runs).
	Extend(ctx context.Context, j *Job, leaseFor time.Duration) error
	// Ack marks the job done.
	Ack(ctx context.Context, j *Job) error
	// Nack returns the job to ready after delay.
	Nack(ctx context.Context, j *Job, delay time.Duration) error
	// DeadLetter parks the job; operators requeue it (Step 18).
	DeadLetter(ctx context.Context, j *Job, reason string) error
	// HasJob reports whether a ready or leased job exists for runID. The
	// stuck-run sweeper uses it to tell a run that is genuinely queued from one
	// stranded with no job behind it (a lost handoff, a crash between CreateRun
	// and Enqueue).
	HasJob(ctx context.Context, runID string) (bool, error)
	Depth(ctx context.Context) (Depth, error)
}

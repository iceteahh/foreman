package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/100xteam-ai/foreman/internal/obs"
	"github.com/100xteam-ai/foreman/internal/queue"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
)

// StuckRunSweeper recovers runs that no job is driving any more.
//
// A run and its job are separate rows (CreateRun then Enqueue), and a lease can
// lapse or be reclaimed while a worker is mid-run. Three things strand a run:
// a replica dying between the two writes, a job acked while its run was still
// `running` (what a reclaimed lease produces), and a lost lease that made this
// replica abandon a run it had already started. In every case the run sits in a
// live status with nothing behind it — it never finishes and nothing reports it.
//
// A run still being worked on holds a leased job, so HasJob tells a stranded run
// from a healthy one. `queued` goes straight back on the queue; a run caught
// mid-flight is walked back along the legal edges (running → evaluating →
// failed → queued) first, which also records why in last_error.
type StuckRunSweeper struct {
	Store store.Store
	Queue queue.Queue
	// MinAge keeps the sweeper off runs that were only just created, which are
	// mid-handoff rather than stranded (default 5 minutes).
	MinAge time.Duration
	// Interval between sweeps (default 5 minutes).
	Interval time.Duration
	// Metrics counts what was recovered; nil-safe.
	Metrics *obs.Metrics
	Logger  *slog.Logger
	Now     func() time.Time
}

func (s *StuckRunSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *StuckRunSweeper) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *StuckRunSweeper) minAge() time.Duration {
	if s.MinAge > 0 {
		return s.MinAge
	}
	return 5 * time.Minute
}

// strandable are the non-terminal statuses a run can be stranded in. A
// `needs_review` run is waiting for a human on purpose, and `failed`/`passed`
// are handed on by the run that produced them, so neither is swept.
var strandable = []task.RunStatus{task.StatusQueued, task.StatusRunning, task.StatusEvaluating}

// recovery is the legal path back to `queued` from each strandable status
// (task.Transition rejects a shortcut).
var recovery = map[task.RunStatus][]task.RunStatus{
	task.StatusQueued:     nil,
	task.StatusRunning:    {task.StatusEvaluating, task.StatusFailed, task.StatusQueued},
	task.StatusEvaluating: {task.StatusFailed, task.StatusQueued},
}

// Once recovers every stranded run and returns how many it put back.
func (s *StuckRunSweeper) Once(ctx context.Context) (int, error) {
	var runs []*task.Run
	for _, st := range strandable {
		got, err := s.Store.ListRunsByStatus(ctx, st)
		if err != nil {
			return 0, err
		}
		runs = append(runs, got...)
	}
	cutoff := s.now().Add(-s.minAge())
	n := 0
	for _, r := range runs {
		if r.CreatedAt.After(cutoff) {
			continue // still being handed off
		}
		if r.StartedAt != nil && r.StartedAt.After(cutoff) {
			continue // a worker started it recently; give it its lease
		}
		live, err := s.Queue.HasJob(ctx, r.ID)
		if err != nil {
			s.log().Warn("checking the queue for a queued run failed", "run_id", r.ID, "err", err)
			continue
		}
		if live {
			continue
		}
		t, err := s.Store.GetTask(ctx, r.TaskID)
		if err != nil {
			s.log().Warn("loading the task of a stranded run failed", "run_id", r.ID, "task_id", r.TaskID, "err", err)
			continue
		}
		const reason = "stranded: no job was driving this run (a lost lease, or a replica that died mid-handoff)"
		failed := false
		for _, to := range recovery[r.Status] {
			if err := s.Store.UpdateRunStatus(ctx, r.ID, to, reason); err != nil {
				s.log().Warn("walking a stranded run back to queued failed", "run_id", r.ID, "to", to, "err", err)
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		if _, err := s.Queue.Enqueue(ctx, queue.Job{RunID: r.ID, TaskID: t.ID, Kind: string(t.Kind), Priority: t.Priority}, 0); err != nil {
			// The run is `queued` with no job again, which is exactly the state
			// this sweeper looks for: the next tick retries.
			s.log().Warn("re-enqueuing a stranded run failed", "run_id", r.ID, "err", err)
			continue
		}
		s.Metrics.StrandedRun(ctx, string(t.Kind))
		s.log().Warn("re-enqueued a run that no job was driving", "run_id", r.ID, "task_id", t.ID,
			"kind", t.Kind, "was", r.Status, "age", s.now().Sub(r.CreatedAt))
		n++
	}
	return n, nil
}

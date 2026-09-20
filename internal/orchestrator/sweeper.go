package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/100xteam-ai/foreman/internal/session"
	"github.com/100xteam-ai/foreman/internal/store"
)

// SessionSweeper enforces retention.sessions_days (design §4.4 lifetime): the
// live transcript snapshots of a task whose newest run has been terminal for
// longer than Retention are deleted. Until then the snapshot doubles as the
// audit artifact and lets an operator `claude --resume` the session.
type SessionSweeper struct {
	Store     store.Store
	Sessions  session.Store
	Retention time.Duration
	// Interval between sweeps (default 1 hour).
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
}

func (s *SessionSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SessionSweeper) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Run sweeps until ctx ends.
func (s *SessionSweeper) Run(ctx context.Context) {
	if s.Retention <= 0 || s.Sessions == nil {
		return
	}
	iv := s.Interval
	if iv <= 0 {
		iv = time.Hour
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Once(ctx); err != nil {
				s.log().Warn("session sweep failed", "err", err)
			} else if n > 0 {
				s.log().Info("session snapshots deleted", "tasks", n)
			}
		}
	}
}

// Once deletes the snapshots of every task terminal for longer than Retention
// and returns how many tasks were swept.
func (s *SessionSweeper) Once(ctx context.Context) (int, error) {
	if s.Retention <= 0 {
		return 0, nil
	}
	ids, err := s.Store.ListTerminalTasks(ctx, s.now().Add(-s.Retention))
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, id := range ids {
		if err := s.Sessions.Delete(ctx, id); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

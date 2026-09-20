package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// FanInSweeper is the crash recovery for fan-out parents (plan Step 21). The
// fast path enqueues the synthesizer as soon as the last child finishes; this
// covers the case where the process died between a child finishing and the
// check, which would otherwise leave the parent waiting forever with every
// child already done.
//
// It is idempotent by construction: the phase advance is a compare-and-swap,
// so sweeping a parent that was already fanned in does nothing.
type FanInSweeper struct {
	Pool *Pool
	// Interval between sweeps (default 5 minutes).
	Interval time.Duration
	Logger   *slog.Logger
}

func (s *FanInSweeper) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	if s.Pool != nil {
		return s.Pool.log()
	}
	return slog.Default()
}

// Run sweeps until ctx ends.
func (s *FanInSweeper) Run(ctx context.Context) {
	if s.Pool == nil {
		return
	}
	iv := s.Interval
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
			if n, err := s.Once(ctx); err != nil {
				s.log().Warn("fan-in sweep failed", "err", err)
			} else if n > 0 {
				s.log().Info("fan-in sweep queued synthesizers", "parents", n)
			}
		}
	}
}

// Once checks every fan-out parent and returns how many synthesizers it queued.
func (s *FanInSweeper) Once(ctx context.Context) (int, error) {
	parents, err := s.Pool.Store.ListFanOutParents(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, id := range parents {
		run, err := s.Pool.maybeFanIn(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if run != nil {
			n++
		}
	}
	return n, errors.Join(errs...)
}

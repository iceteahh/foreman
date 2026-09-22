package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/100xteam-ai/foreman/internal/store"
)

// Lease names for the periodic jobs that must not run twice (plan Step 22).
const (
	LeaseEscalator = "review-escalator"
	LeaseSessions  = "session-sweeper"
	LeaseFanIn     = "fan-in-sweeper"
	LeaseStuckRuns = "stuck-run-sweeper"
	LeaseCron      = "cron"
)

// ReplicaID identifies this process in a lease row: hostname, pid and a random
// suffix. The suffix matters — two replicas can share a hostname (a restarted
// pod keeps its name), and a stale lease held by a dead predecessor would
// otherwise be renewed by its successor instead of being contended for.
func ReplicaID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Periodic runs one background job on an interval, on exactly one replica at a
// time (design §10 "stateless orchestrator replicas, leader-free leasing").
//
// There is no election: each tick asks for a named lease, and only the holder
// does the work. A replica that dies holding one costs the job a single TTL of
// delay — never a stuck queue, and never a coordinator to operate.
//
// With Leases nil (a single-node install on SQLite) every tick runs, which is
// the correct behaviour for one process.
type Periodic struct {
	// Name is the lease name; What is the human label in logs.
	Name, What string
	// Interval between ticks.
	Interval time.Duration
	// TTL is how long a won lease is held (default 3×Interval, so a tick that
	// overruns does not hand the job to a second replica mid-flight).
	TTL time.Duration
	// Once does the work and returns how much of it there was.
	Once func(context.Context) (int, error)
	// Leases gates each tick; nil runs every tick.
	Leases store.Leases
	Owner  string
	Logger *slog.Logger
}

func (p *Periodic) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

func (p *Periodic) ttl() time.Duration {
	if p.TTL > 0 {
		return p.TTL
	}
	if p.Interval > 0 {
		return 3 * p.Interval
	}
	return 15 * time.Minute
}

// Run ticks until ctx ends, then releases the lease so a surviving replica
// picks the job up immediately instead of waiting out the TTL.
func (p *Periodic) Run(ctx context.Context) {
	if p.Once == nil || p.Interval <= 0 {
		return
	}
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	defer p.release()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

func (p *Periodic) tick(ctx context.Context) {
	if !p.acquire(ctx) {
		return
	}
	n, err := p.Once(ctx)
	switch {
	case err != nil:
		p.log().Warn(p.What+" failed", "err", err)
	case n > 0:
		p.log().Info(p.What+" done", "count", n)
	}
}

// acquire reports whether this replica may do the work this tick.
func (p *Periodic) acquire(ctx context.Context) bool {
	if p.Leases == nil {
		return true
	}
	ok, err := p.Leases.AcquireLease(ctx, p.Name, p.Owner, p.ttl())
	if err != nil {
		// A lease we cannot read is not a licence to run twice: skipping one
		// tick of a periodic job is always cheaper than doubling it.
		p.log().Warn("lease check failed; skipping this tick", "job", p.Name, "err", err)
		return false
	}
	return ok
}

func (p *Periodic) release() {
	if p.Leases == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Leases.ReleaseLease(ctx, p.Name, p.Owner); err != nil {
		p.log().Debug("releasing lease failed", "job", p.Name, "err", err)
	}
}

// LeaseGate returns a function that reports whether this replica owns the
// named lease right now. `harness serve` hands one to the cron scheduler so a
// schedule fires once across the fleet rather than once per replica — the
// failure mode that turns a nightly report into N nightly reports and N
// budgets. Sub-names keep one entry's gate independent of another's.
func LeaseGate(leases store.Leases, owner string, ttl time.Duration, logger *slog.Logger) func(context.Context, string) bool {
	if leases == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, sub string) bool {
		name := LeaseCron
		if sub != "" {
			name += ":" + sub
		}
		ok, err := leases.AcquireLease(ctx, name, owner, ttl)
		if err != nil {
			logger.Warn("cron lease check failed; skipping this firing", "cron", sub, "err", err)
			return false
		}
		return ok
	}
}

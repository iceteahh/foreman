package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLeases is an in-memory store.Leases with the same compare-and-swap
// semantics as the SQL ones.
type fakeLeases struct {
	mu    sync.Mutex
	held  map[string]string
	until map[string]time.Time
	now   func() time.Time
	fail  error
	calls int32
}

func newLeases(now func() time.Time) *fakeLeases {
	return &fakeLeases{held: map[string]string{}, until: map[string]time.Time{}, now: now}
}

func (f *fakeLeases) AcquireLease(_ context.Context, name, owner string, ttl time.Duration) (bool, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return false, f.fail
	}
	cur, ok := f.held[name]
	if ok && cur != owner && f.until[name].After(f.now()) {
		return false, nil
	}
	f.held[name] = owner
	f.until[name] = f.now().Add(ttl)
	return true, nil
}

func (f *fakeLeases) ReleaseLease(_ context.Context, name, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[name] == owner {
		delete(f.held, name)
		delete(f.until, name)
	}
	return nil
}

func TestPeriodicRunsOnOneReplicaOnly(t *testing.T) {
	now := time.Now()
	leases := newLeases(func() time.Time { return now })
	var a, b atomic.Int32
	mk := func(owner string, n *atomic.Int32) *Periodic {
		return &Periodic{Name: "sweep", What: "sweep", Interval: 5 * time.Millisecond, TTL: time.Minute,
			Leases: leases, Owner: owner, Logger: quiet,
			Once: func(context.Context) (int, error) { n.Add(1); return 0, nil }}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, p := range []*Periodic{mk("replica-a", &a), mk("replica-b", &b)} {
		wg.Add(1)
		go func(p *Periodic) { defer wg.Done(); p.Run(ctx) }(p)
	}
	time.Sleep(120 * time.Millisecond)
	// Snapshot before cancelling. Run releases its lease on the way out so a
	// surviving replica picks the job up immediately rather than waiting out
	// the TTL, and the sibling's select can take one more tick before it
	// observes ctx.Done() — so it legitimately wins the freed lease during
	// teardown. Exclusivity is a property of steady state, not of the handoff
	// that shutdown is supposed to perform.
	aRan, bRan := a.Load(), b.Load()
	cancel()
	wg.Wait()

	total := aRan + bRan
	if total == 0 {
		t.Fatal("neither replica ran the job")
	}
	// Whoever wins the lease keeps it for the TTL: the work must not be split.
	if aRan > 0 && bRan > 0 {
		t.Fatalf("both replicas did the work: a=%d b=%d", aRan, bRan)
	}
}

func TestPeriodicWithoutLeasesAlwaysRuns(t *testing.T) {
	var n atomic.Int32
	p := &Periodic{Name: "sweep", Interval: 5 * time.Millisecond, Logger: quiet,
		Once: func(context.Context) (int, error) { n.Add(1); return 1, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	if n.Load() == 0 {
		t.Fatal("a single-node install must run every tick")
	}
}

// A lease we cannot read is not a licence to run twice: skipping a tick of a
// periodic job is always cheaper than doubling it.
func TestPeriodicSkipsTheTickWhenTheLeaseCheckFails(t *testing.T) {
	now := time.Now()
	leases := newLeases(func() time.Time { return now })
	leases.fail = errors.New("database is down")
	var n atomic.Int32
	p := &Periodic{Name: "sweep", Interval: 5 * time.Millisecond, Leases: leases, Owner: "a", Logger: quiet,
		Once: func(context.Context) (int, error) { n.Add(1); return 0, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	if n.Load() != 0 {
		t.Fatalf("ran %d times despite an unreadable lease", n.Load())
	}
	if atomic.LoadInt32(&leases.calls) == 0 {
		t.Error("the lease was never checked")
	}
}

// A graceful shutdown hands the job over immediately instead of leaving the
// fleet idle for a whole TTL.
func TestPeriodicReleasesItsLeaseOnShutdown(t *testing.T) {
	now := time.Now()
	leases := newLeases(func() time.Time { return now })
	p := &Periodic{Name: "sweep", Interval: 5 * time.Millisecond, TTL: time.Hour, Leases: leases, Owner: "a", Logger: quiet,
		Once: func(context.Context) (int, error) { return 0, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	p.Run(ctx)
	if ok, err := leases.AcquireLease(context.Background(), "sweep", "b", time.Minute); err != nil || !ok {
		t.Fatalf("the lease was not released on shutdown: ok=%v err=%v", ok, err)
	}
}

func TestLeaseGateSeparatesCronEntries(t *testing.T) {
	now := time.Now()
	leases := newLeases(func() time.Time { return now })
	gate := LeaseGate(leases, "replica-a", time.Minute, quiet)
	other := LeaseGate(leases, "replica-b", time.Minute, quiet)
	ctx := context.Background()

	if !gate(ctx, "nightly-report") {
		t.Fatal("the first replica did not win the firing")
	}
	// Otherwise a nightly report becomes N nightly reports and N budgets.
	if other(ctx, "nightly-report") {
		t.Error("a second replica fired the same cron entry")
	}
	// A different entry is independent.
	if !other(ctx, "hourly-triage") {
		t.Error("one entry's gate blocked another's")
	}
	if LeaseGate(nil, "a", time.Minute, quiet) != nil {
		t.Error("a nil lease store must produce no gate (single process fires everything)")
	}
}

func TestReplicaIDIsUniquePerProcess(t *testing.T) {
	a, b := ReplicaID(), ReplicaID()
	if a == b {
		t.Fatalf("two replicas on one host share the lease owner %q and would renew each other's leases", a)
	}
}

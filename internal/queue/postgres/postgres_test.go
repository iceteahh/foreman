//go:build postgres

// These tests need a real database and no tokens; see the store package's
// test file for how to run them (`make postgres-test`).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
)

func open(t *testing.T) (*Queue, *time.Time) {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q, err := New(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE TABLE jobs"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	q.SetClock(func() time.Time { return now })
	return q, &now
}

func job(run, kind string, prio int) queue.Job {
	return queue.Job{RunID: run, TaskID: "tsk_" + run, Kind: kind, Priority: prio}
}

func TestLeaseOrderAndKinds(t *testing.T) {
	q, _ := open(t)
	ctx := context.Background()
	for _, j := range []queue.Job{job("a", "code_fix", 1), job("b", "code_fix", 9), job("c", "report", 5)} {
		if _, err := q.Enqueue(ctx, j, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Lease(ctx, []string{"triage"}, time.Minute); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("wrong kind leased: %v", err)
	}
	j, err := q.Lease(ctx, []string{"code_fix"}, time.Minute)
	if err != nil || j.RunID != "b" || j.Attempts != 1 || j.LeaseToken == "" {
		t.Fatalf("first lease %+v %v", j, err)
	}
	j2, err := q.Lease(ctx, nil, time.Minute)
	if err != nil || j2.RunID != "c" {
		t.Fatalf("second lease (any kind, highest priority) %+v %v", j2, err)
	}
	j3, _ := q.Lease(ctx, nil, time.Minute)
	if j3.RunID != "a" {
		t.Fatalf("third %+v", j3)
	}
	if _, err := q.Lease(ctx, nil, time.Minute); !errors.Is(err, queue.ErrEmpty) {
		t.Errorf("queue should be drained: %v", err)
	}
	d, _ := q.Depth(ctx)
	if d.Leased != 3 || d.Ready != 0 {
		t.Errorf("depth %+v", d)
	}
}

func TestAckNackDeadAndLeaseTokens(t *testing.T) {
	q, now := open(t)
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), 0); err != nil {
		t.Fatal(err)
	}
	j, _ := q.Lease(ctx, nil, time.Minute)
	stale := *j
	if err := q.Nack(ctx, j, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	// The lease token is what stops a replica whose lease expired mid-run from
	// acking work another replica has already taken over.
	if err := q.Ack(ctx, &stale); !errors.Is(err, queue.ErrLeaseLost) {
		t.Errorf("stale token accepted: %v", err)
	}
	if _, err := q.Lease(ctx, nil, time.Minute); !errors.Is(err, queue.ErrEmpty) {
		t.Error("nacked job visible before delay")
	}
	*now = now.Add(31 * time.Second)
	j, err := q.Lease(ctx, nil, time.Minute)
	if err != nil || j.Attempts != 2 {
		t.Fatalf("release %+v %v", j, err)
	}
	if err := q.Extend(ctx, j, 5*time.Minute); err != nil || !j.LeasedUntil.Equal(now.Add(5*time.Minute)) {
		t.Errorf("extend %v %v", err, j.LeasedUntil)
	}
	if err := q.DeadLetter(ctx, j, "exhausted"); err != nil {
		t.Fatal(err)
	}
	d, _ := q.Depth(ctx)
	if d.Dead != 1 || d.Ready != 0 || d.Leased != 0 {
		t.Errorf("depth %+v", d)
	}
	if _, err := q.Enqueue(ctx, job("b", "code_fix", 0), 0); err != nil {
		t.Fatal(err)
	}
	j, _ = q.Lease(ctx, nil, time.Minute)
	if err := q.Ack(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, j); !errors.Is(err, queue.ErrLeaseLost) {
		t.Error("double ack accepted")
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	q, now := open(t)
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), 0); err != nil {
		t.Fatal(err)
	}
	j1, _ := q.Lease(ctx, nil, time.Minute)
	*now = now.Add(2 * time.Minute)
	j2, err := q.Lease(ctx, nil, time.Minute)
	if err != nil || j2.ID != j1.ID || j2.Attempts != 2 {
		t.Fatalf("expired lease not reclaimed: %+v %v", j2, err)
	}
	if err := q.Ack(ctx, j1); !errors.Is(err, queue.ErrLeaseLost) {
		t.Error("old holder could still ack")
	}
	if err := q.Ack(ctx, j2); err != nil {
		t.Error(err)
	}
}

func TestDuplicateActiveRunRejected(t *testing.T) {
	q, _ := open(t)
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), 0); err == nil {
		t.Error("same run enqueued twice while active")
	}
}

func TestDelayAndDepthAge(t *testing.T) {
	q, now := open(t)
	ctx := context.Background()
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Lease(ctx, nil, time.Minute); !errors.Is(err, queue.ErrEmpty) {
		t.Error("delayed job leased early")
	}
	*now = now.Add(2 * time.Hour)
	d, _ := q.Depth(ctx)
	if d.Ready != 1 || d.OldestReadyAge != time.Hour {
		t.Errorf("depth %+v", d)
	}
}

// This is the property the whole scaled topology rests on: N orchestrator
// replicas polling one table must never hand the same run to two workers.
// SKIP LOCKED is what makes them step over each other instead of serialising.
func TestConcurrentLeasesAreExclusive(t *testing.T) {
	q, _ := open(t)
	q.SetClock(time.Now)
	ctx := context.Background()
	const n = 40
	for i := 0; i < n; i++ {
		if _, err := q.Enqueue(ctx, job(fmt.Sprintf("run%02d", i), "code_fix", 0), 0); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := q.Lease(ctx, nil, time.Minute)
				if errors.Is(err, queue.ErrEmpty) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
				if err := q.Ack(ctx, j); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("leased %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %d leased %d times", id, c)
		}
	}
}

// A job must be leasable the instant it is enqueued. In SQLite these
// timestamps are compared as text and a trimmed fraction once made a job
// invisible; Postgres compares instants, and this pins that it stays that way.
func TestLeaseSeesAJustEnqueuedJob(t *testing.T) {
	q, now := open(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 16, 9, 55, 53, 0, time.UTC)
	*now = base.Add(327 * time.Microsecond)
	if _, err := q.Enqueue(ctx, job("a", "code_fix", 0), 0); err != nil {
		t.Fatal(err)
	}
	*now = base.Add(327852 * time.Nanosecond)
	if _, err := q.Lease(ctx, nil, time.Minute); err != nil {
		t.Fatalf("a job enqueued microseconds earlier was invisible: %v", err)
	}
}

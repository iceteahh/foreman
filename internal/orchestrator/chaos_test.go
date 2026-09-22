package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/budget"
	"github.com/100xteam-ai/foreman/internal/queue"
	"github.com/100xteam-ai/foreman/internal/task"
)

// leaseLosingQueue answers Extend with ErrLeaseLost from the nth call on,
// standing in for a lease another replica reclaimed while this run was in
// flight.
type leaseLosingQueue struct {
	queue.Queue
	mu         sync.Mutex
	extends    int
	loseAfter  int
	extendErr  error
	nackedLost bool
}

func (q *leaseLosingQueue) Extend(ctx context.Context, j *queue.Job, d time.Duration) error {
	q.mu.Lock()
	q.extends++
	n, lose := q.extends, q.loseAfter
	err := q.extendErr
	q.mu.Unlock()
	if lose > 0 && n >= lose {
		if err == nil {
			err = queue.ErrLeaseLost
		}
		return err
	}
	return q.Queue.Extend(ctx, j, d)
}

func (q *leaseLosingQueue) Nack(ctx context.Context, j *queue.Job, d time.Duration) error {
	q.mu.Lock()
	lost := q.loseAfter > 0 && q.extends >= q.loseAfter
	if lost {
		q.nackedLost = true
	}
	q.mu.Unlock()
	if lost {
		return queue.ErrLeaseLost
	}
	return q.Queue.Nack(ctx, j, d)
}

func (q *leaseLosingQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.extends
}

// A lease reclaimed mid-run cancels the run: two workers driving one run
// duplicate its side effects. The job is left to whoever now owns it, and
// ErrLeaseLost from the terminal Nack is not reported as a processing error.
func TestChaosLeaseLostMidRunCancelsTheRun(t *testing.T) {
	// The worker sleeps well past the point where the lease is lost.
	bin, _ := fakeClaude(t, `sleep 20`)
	e := newEnv(t, bin)
	lq := &leaseLosingQueue{Queue: e.q, loseAfter: 1}
	e.pool.Queue = lq
	e.pool.Config.Lease = 300 * time.Millisecond // heartbeat every 150ms

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := e.spec("sh check.sh")
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 60000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	tk, r1, err := e.sub.Submit(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
		t.Fatalf("a lost lease must not surface as a processing error: %v", err)
	}
	// The run was abandoned well before the worker's own 60s timeout.
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("the run kept going for %s after the lease was lost", elapsed)
	}
	if lq.count() == 0 {
		t.Error("the heartbeat never ran, so the test proved nothing")
	}
	got, err := e.store.GetRun(ctx, r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever status it reached, the run must not have been delivered: the
	// replica that owns the lease is the one allowed to finish it.
	if got.Status == task.StatusDelivered {
		t.Errorf("a run whose lease was lost was delivered anyway: %+v", got)
	}
	if got.Status.Terminal() {
		t.Errorf("an abandoned run was given a terminal status: %s", got.Status)
	}

	// Abandoning is only safe because the run stays recoverable. The abandoned
	// job is still leased, so it lapses and the next worker picks it up; that
	// worker finds the run no longer queued and acks the stale job, which is
	// the moment the run is genuinely stranded — and what the sweeper is for.
	e.pool.Queue = e.q
	bin2, _ := fakeClaude(t, `touch src/fixed; cat "`+fixture("result_success.json")+`"`)
	e.pool.Runner.Bin = bin2
	time.Sleep(2 * e.pool.Config.Lease)
	if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
		t.Fatalf("reclaiming the lapsed job: %v", err)
	}
	if live, err := e.q.HasJob(ctx, r1.ID); err != nil || live {
		t.Fatalf("the stale job was not acked (%v, %v); the run is not stranded yet", live, err)
	}
	sweeper := &StuckRunSweeper{Store: e.store, Queue: e.q, MinAge: time.Minute, Logger: quiet,
		Now: func() time.Time { return time.Now().Add(2 * time.Minute) }}
	if n, err := sweeper.Once(ctx); err != nil || n != 1 {
		t.Fatalf("the sweeper recovered %d abandoned runs (%v), want 1", n, err)
	}
	if final := e.drive(t, tk.ID); final.Status != task.StatusDelivered {
		t.Fatalf("the recovered run ended %s (%q)", final.Status, final.LastError)
	}
}

// Repeated extend failures are the same hazard arriving slowly: after three
// misses the lease is about to lapse, so the run stops rather than racing the
// worker that will reclaim it.
func TestChaosRepeatedHeartbeatFailureCancelsTheRun(t *testing.T) {
	bin, _ := fakeClaude(t, `sleep 20`)
	e := newEnv(t, bin)
	lq := &leaseLosingQueue{Queue: e.q, loseAfter: 1, extendErr: errors.New("database is locked")}
	e.pool.Queue = lq
	e.pool.Config.Lease = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := e.spec("sh pass.sh")
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 60000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	if _, _, err := e.sub.Submit(ctx, spec); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := e.pool.ProcessOne(ctx, ctx); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("process: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Errorf("the run ignored %s of heartbeat failures", elapsed)
	}
	if n := lq.count(); n < 3 {
		t.Errorf("gave up after %d extends; three consecutive failures is the bar", n)
	}
}

// A dead run from an earlier phase cannot be requeued: its replacement would
// carry the old phase, redoing planning after the plan was approved and
// throwing the approval away.
func TestChaosRequeueRefusesAStalePhase(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()

	spec := e.spec("sh fail.sh")
	spec.Kind = task.KindCodeFixPlanned
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	tk, _, err := e.sub.Submit(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	dead := e.drive(t, tk.ID)
	if dead.Status != task.StatusDead {
		t.Fatalf("expected a dead run to requeue, got %s (%q)", dead.Status, dead.LastError)
	}
	if dead.Phase != 1 {
		t.Fatalf("the dead run is in phase %d, not the first", dead.Phase)
	}

	// The task moves on without it (a human approved the plan elsewhere).
	if err := e.store.UpdateTaskPhase(ctx, tk.ID, 1, 2); err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Requeue(ctx, dead.ID, "operator", "try again")
	if err == nil {
		t.Fatal("requeuing a run from an abandoned phase was accepted; it would rewind the task")
	}
	for _, want := range []string{"phase", "rewind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not explain itself (%q missing): %v", want, err)
		}
	}
}

// A run left `queued` with no job behind it is invisible: nothing leases it and
// nothing reports it. The sweeper puts it back on the queue.
func TestChaosStrandedQueuedRunIsSweptBack(t *testing.T) {
	bin, _ := fakeClaude(t, `touch src/fixed; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r1, err := e.sub.Submit(ctx, e.spec("sh check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Strand it: take the job away while the run stays queued, exactly as a
	// replica dying between CreateRun and Enqueue would leave things.
	job, err := e.q.Lease(ctx, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.Ack(ctx, job); err != nil {
		t.Fatal(err)
	}
	if live, err := e.q.HasJob(ctx, r1.ID); err != nil || live {
		t.Fatalf("the run still has a job (%v, %v); the test proved nothing", live, err)
	}

	sweeper := &StuckRunSweeper{Store: e.store, Queue: e.q, MinAge: time.Minute, Logger: quiet,
		Now: func() time.Time { return time.Now().Add(2 * time.Minute) }}
	n, err := sweeper.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d runs, want 1", n)
	}
	if live, err := e.q.HasJob(ctx, r1.ID); err != nil || !live {
		t.Fatalf("the stranded run was not re-enqueued (%v, %v)", live, err)
	}
	// And it now runs to completion.
	if got := e.drive(t, tk.ID); got.Status != task.StatusDelivered {
		t.Fatalf("the recovered run ended %s (%q)", got.Status, got.LastError)
	}

	// A second sweep finds nothing: a run with a live job is not stranded.
	if n, err := sweeper.Once(ctx); err != nil || n != 0 {
		t.Errorf("the sweeper re-enqueued a healthy run: %d %v", n, err)
	}
}

// A run created moments ago is mid-handoff, not stranded. Sweeping it would
// race the enqueue that is about to happen.
func TestStuckSweeperLeavesFreshRunsAlone(t *testing.T) {
	bin, _ := fakeClaude(t, `cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	_, r1, err := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	job, _ := e.q.Lease(ctx, nil, time.Minute)
	if err := e.q.Ack(ctx, job); err != nil {
		t.Fatal(err)
	}
	sweeper := &StuckRunSweeper{Store: e.store, Queue: e.q, MinAge: time.Hour, Logger: quiet}
	if n, err := sweeper.Once(ctx); err != nil || n != 0 {
		t.Fatalf("swept %d fresh runs (%v); a run younger than MinAge is mid-handoff", n, err)
	}
	if live, _ := e.q.HasJob(ctx, r1.ID); live {
		t.Error("a fresh run was re-enqueued")
	}
}

// The daily ceiling is hard, not advisory. Check alone reads the spend so far,
// so runs starting together all see room and all start: the day ends N
// max-costs over its limit. Reserving each run's own ceiling up front is what
// makes the limit hold, and settling returns whatever the run did not spend.
func TestChaosBudgetCeilingIsHard(t *testing.T) {
	bin, _ := fakeClaude(t, `touch src/fixed; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	// Room for exactly two claims of 0.4.
	pager := withOps(e, map[string]float64{"code_fix": 1.0, budget.Global: 1.0})
	_ = pager
	ctx := context.Background()
	day := budget.Day(time.Now())

	first, err := e.pool.Budget.Reserve(ctx, "code_fix", 0.4)
	if err != nil || first == nil {
		t.Fatalf("first reservation: %v", err)
	}
	second, err := e.pool.Budget.Reserve(ctx, "code_fix", 0.4)
	if err != nil || second == nil {
		t.Fatalf("second reservation: %v", err)
	}
	// Both claims are booked even though nothing has been spent yet, which is
	// exactly the headroom a soft ceiling would hand out twice.
	if spent, _, _ := e.store.GetSpend(ctx, "code_fix", day); spent < 0.79 || spent > 0.81 {
		t.Errorf("booked %v, want both claims (0.80) held", spent)
	}
	if _, err := e.pool.Budget.Reserve(ctx, "code_fix", 0.4); err == nil {
		t.Fatal("a third claim fitted inside a ceiling that only had room for two")
	} else if !errors.Is(err, budget.ErrExhausted) {
		t.Errorf("refusal is not an ErrExhausted: %v", err)
	}
	// A refused claim is handed back, not left booked.
	if spent, _, _ := e.store.GetSpend(ctx, "code_fix", day); spent < 0.79 || spent > 0.81 {
		t.Errorf("a refused claim stayed booked: %v", spent)
	}

	// Settling returns the unspent part, and the room comes back.
	if err := e.pool.Budget.Settle(ctx, first, 0.05); err != nil {
		t.Fatal(err)
	}
	spent, runs, _ := e.store.GetSpend(ctx, "code_fix", day)
	if spent < 0.44 || spent > 0.46 {
		t.Errorf("after settling one claim at 0.05 the day reads %v, want ~0.45", spent)
	}
	if runs != 2 {
		t.Errorf("runs = %d; a reservation counts its run once, settling must not count it again", runs)
	}
	if _, err := e.pool.Budget.Reserve(ctx, "code_fix", 0.4); err != nil {
		t.Errorf("the refunded headroom was not reusable: %v", err)
	}

	// A run whose own ceiling is larger than the day's could never start, and
	// saying "budget exhausted" would send an operator looking at today's
	// spend instead of at the misconfiguration.
	_, err = e.pool.Budget.Reserve(ctx, "code_fix", 5.0)
	if err == nil {
		t.Fatal("a claim larger than the whole daily ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "can never start") {
		t.Errorf("the refusal does not name the misconfiguration: %v", err)
	}
}

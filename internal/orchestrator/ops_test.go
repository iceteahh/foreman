package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/budget"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/deadletter"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/runner"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// fakePager captures pages so the chaos test can assert one fired.
type fakePager struct {
	mu    sync.Mutex
	pages []string
}

func (p *fakePager) Page(_ context.Context, subject, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = append(p.pages, subject+"\n"+detail)
}

func (p *fakePager) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.pages...)
}

// withOps adds the M3 ledger and dead-letter recorder to a test env.
func withOps(e *env, limits map[string]float64) *fakePager {
	pager := &fakePager{}
	if limits != nil {
		l := budget.New(e.store, limits, quiet)
		l.Pager = pager
		e.pool.Budget = l
	}
	e.pool.Dead = &deadletter.Recorder{Store: e.store, Pager: pager, Logger: quiet}
	e.pool.DeadStore = e.store
	return pager
}

// TestChaosDeadLetterPagesAndRequeues is the Step 18 "done when": fail a task
// until its retries are exhausted, then observe the DLQ row, the page with the
// failure evidence, and a successful `harness requeue`.
func TestChaosDeadLetterPagesAndRequeues(t *testing.T) {
	// The fake CLI edits the workspace but never creates src/fixed, so
	// check.sh fails every attempt.
	bin, _ := fakeClaude(t, `printf 'package a\n// attempt\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	pager := withOps(e, nil)
	ctx := context.Background()
	tk, r1, err := e.sub.Submit(ctx, e.spec("sh check.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// max_retries is 1, so attempt 1 fails, attempt 2 fails and dies.
	final := e.drive(t, tk.ID)
	if final.Status != task.StatusDead {
		t.Fatalf("final status %s (%q)", final.Status, final.LastError)
	}
	if final.Attempt != 2 || final.RetryOf != r1.ID {
		t.Fatalf("chain %+v", final)
	}

	// The dead letter is recorded with the evidence an operator needs.
	entry, err := e.store.GetEntry(ctx, final.ID)
	if err != nil {
		t.Fatalf("no dead-letter row: %v", err)
	}
	if entry.Kind != string(task.KindCodeFix) || entry.Attempt != 2 || !entry.Open() {
		t.Errorf("entry %+v", entry)
	}
	if !strings.Contains(entry.Feedback, "acceptance_commands") || !strings.Contains(entry.Feedback, "src/fixed missing") {
		t.Errorf("entry feedback lacks the check evidence: %q", entry.Feedback)
	}
	if entry.EventLogURI == "" {
		t.Error("entry has no event log pointer")
	}
	if entry.PagedAt == nil {
		t.Error("entry not marked paged")
	}
	pages := pager.all()
	if len(pages) != 1 {
		t.Fatalf("pages %d: %v", len(pages), pages)
	}
	for _, want := range []string{"dead-lettered", final.ID, "harness requeue " + final.ID, "harness replay " + final.ID, "src/fixed missing"} {
		if !strings.Contains(pages[0], want) {
			t.Errorf("page missing %q:\n%s", want, pages[0])
		}
	}
	if open, err := e.store.ListOpen(ctx, 10); err != nil || len(open) != 1 || open[0].RunID != final.ID {
		t.Errorf("open dead letters %+v %v", open, err)
	}

	// Requeue: a fresh attempt counter, the session resumed, the entry closed.
	next, err := e.pool.Requeue(ctx, final.ID, "operator", "the file is src/fixed, create it")
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if next.Attempt != 1 {
		t.Errorf("requeue must reset the attempt counter, got %d", next.Attempt)
	}
	if next.SessionMode != task.SessionContinue || next.SessionID != final.SessionID {
		t.Errorf("requeued run %+v", next)
	}
	if !strings.Contains(next.Prompt, "the file is src/fixed") || !strings.Contains(next.Prompt, "src/fixed missing") {
		t.Errorf("requeue prompt lacks the note or the evidence:\n%s", next.Prompt)
	}
	entry, _ = e.store.GetEntry(ctx, final.ID)
	if entry.Open() || entry.RequeueRunID != next.ID {
		t.Errorf("entry not closed by the requeue: %+v", entry)
	}
	if open, _ := e.store.ListOpen(ctx, 10); len(open) != 0 {
		t.Errorf("requeued entry still open: %+v", open)
	}

	// A second requeue of the same dead run is refused while the retry is live.
	if _, err := e.pool.Requeue(ctx, final.ID, "operator", ""); err == nil {
		t.Error("requeue accepted while a live run exists")
	}
	// And a run that is not dead cannot be requeued at all.
	if _, err := e.pool.Requeue(ctx, r1.ID, "operator", ""); err == nil {
		t.Error("requeue accepted a non-dead run")
	}

	// The requeued attempt runs with a worker that now satisfies the check.
	bin2, _ := fakeClaude(t, `touch src/fixed; cat "`+fixture("result_resumed.json")+`"`)
	e.pool.Runner.Bin = bin2
	got := e.drive(t, tk.ID)
	if got.ID != next.ID || got.Status != task.StatusDelivered {
		t.Fatalf("requeued run %+v", got)
	}
}

// TestBudgetCeilingStopsLeasingAndRecordsSpend covers Step 15 end to end: the
// ledger records what a run cost, the kind's ceiling stops further leasing,
// and the global breach opens the circuit breaker and pages.
func TestBudgetCeilingStopsLeasingAndRecordsSpend(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n\nfunc Fixed() {}\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	// The fixture costs 0.0196885 USD, so a 0.015 ceiling is breached by one run.
	pager := withOps(e, map[string]float64{"code_fix": 0.015, budget.Global: 1})
	ctx := context.Background()

	tk, _, err := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDelivered {
		t.Fatalf("first run %s (%q)", got.Status, got.LastError)
	}
	// Spend is recorded for the kind and globally.
	spent, runs, err := e.store.GetSpend(ctx, "code_fix", budget.Day(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 || spent < 0.019 || spent > 0.02 {
		t.Errorf("kind spend %v over %d runs", spent, runs)
	}
	if g, _, _ := e.store.GetSpend(ctx, budget.Global, budget.Day(time.Now())); g != spent {
		t.Errorf("global spend %v want %v", g, spent)
	}

	// The kind is now over its ceiling: a new task is queued but never leased.
	tk2, r2, err := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	processed, err := e.pool.ProcessOne(ctx, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if processed {
		t.Error("a saturated kind was leased anyway")
	}
	cur, _ := e.store.GetRun(ctx, r2.ID)
	if cur.Status != task.StatusQueued {
		t.Errorf("run %s advanced past queued", cur.Status)
	}
	if kinds := e.pool.leasableKinds(ctx); kinds == nil {
		t.Error("leasableKinds must exclude the exhausted kind")
	} else {
		for _, k := range kinds {
			if k == "code_fix" {
				t.Error("exhausted kind still leasable")
			}
		}
	}
	_ = tk2

	// Raising the ceiling lets it run again: nothing about the queued job changed.
	e.pool.Budget.Limits["code_fix"] = 10
	got2 := e.drive(t, tk2.ID)
	if got2.Status != task.StatusDelivered {
		t.Fatalf("second run after raising the ceiling: %s (%q)", got2.Status, got2.LastError)
	}

	// Now breach the global ceiling: the breaker opens, pages once, and no
	// kind is leasable until the day rolls over.
	e.pool.Budget.Limits[budget.Global] = 0.01
	if err := e.pool.Budget.Record(ctx, "code_fix", 0.02); err != nil {
		t.Fatal(err)
	}
	if !e.pool.Budget.BreakerOpen() {
		t.Fatal("global breaker did not open")
	}
	if kinds := e.pool.leasableKinds(ctx); kinds == nil || len(kinds) != 0 {
		t.Errorf("breaker open but leasableKinds = %v", kinds)
	}
	tk3, r3, _ := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if processed, _ := e.pool.ProcessOne(ctx, ctx); processed {
		t.Error("a job was leased with the global breaker open")
	}
	cur3, _ := e.store.GetRun(ctx, r3.ID)
	if cur3.Status != task.StatusQueued {
		t.Errorf("run %s advanced with the breaker open", cur3.Status)
	}
	_ = tk3
	var breachPage bool
	for _, p := range pager.all() {
		if strings.Contains(p, "global daily budget exhausted") {
			breachPage = true
		}
	}
	if !breachPage {
		t.Errorf("no page for the global breach: %v", pager.all())
	}
}

// Rate limiting backs off exponentially in the job's attempt count and sheds
// concurrency, instead of hammering a throttled account (design §9).
func TestRateLimitBacksOffExponentially(t *testing.T) {
	e := newEnv(t, "/nonexistent")
	e.pool.init()
	e.pool.Config.APIErrorBackoff = time.Minute
	e.pool.Config.MaxBackoff = 8 * time.Minute

	plain := &runner.Result{}
	limited := &runner.Result{RateLimited: true}
	// Without rate limiting the delay is the flat api_error backoff.
	for _, attempts := range []int{1, 3, 9} {
		if got := e.pool.backoff(&queue.Job{Attempts: attempts}, plain); got != time.Minute {
			t.Errorf("attempt %d: %s, want 1m", attempts, got)
		}
	}
	// A rate limit doubles per attempt, capped by MaxBackoff.
	want := map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 9: 8 * time.Minute}
	for attempts, expect := range want {
		if got := e.pool.backoff(&queue.Job{Attempts: attempts}, limited); got != expect {
			t.Errorf("rate-limited attempt %d: %s, want %s", attempts, got, expect)
		}
	}
	// Backing off also sheds: leasing is paused for the same window.
	if paused, until := e.pool.shedding(); !paused || time.Until(until) < 7*time.Minute {
		t.Errorf("rate limiting did not shed concurrency (paused=%v until=%s)", paused, until)
	}
	// A nil result never sheds or backs off oddly.
	if got := e.pool.backoff(&queue.Job{Attempts: 2}, nil); got != time.Minute {
		t.Errorf("nil result backoff %s", got)
	}
}

func TestShedPausesLeasing(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n\nfunc Fixed() {}\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, _, err := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// A shed window pauses leasing entirely; the job stays ready.
	e.pool.init()
	e.pool.shed(2 * time.Second)
	if paused, _ := e.pool.shedding(); !paused {
		t.Fatal("shed did not pause leasing")
	}
	if processed, _ := e.pool.ProcessOne(ctx, ctx); processed {
		t.Error("leased a job during the shed window")
	}
	if d, _ := e.q.Depth(ctx); d.Ready != 1 {
		t.Errorf("queue depth %+v", d)
	}
	// Once the window passes, the job runs normally.
	e.pool.shedMu.Lock()
	e.pool.shedUntil = time.Now().Add(-time.Second)
	e.pool.shedMu.Unlock()
	if got := e.drive(t, tk.ID); got.Status != task.StatusDelivered {
		t.Fatalf("run after the shed window: %s (%q)", got.Status, got.LastError)
	}
}

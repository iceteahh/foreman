package budget

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

// memStore is an in-memory budget.Store; the SQLite implementation is covered
// in internal/store/sqlite.
type memStore struct {
	mu   sync.Mutex
	usd  map[string]float64
	runs map[string]int
	err  error
}

func newMem() *memStore { return &memStore{usd: map[string]float64{}, runs: map[string]int{}} }

func (m *memStore) AddSpend(_ context.Context, key, day string, usd float64, runs int) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	k := key + "/" + day
	m.usd[k] += usd
	m.runs[k] += runs
	return m.usd[k], nil
}

func (m *memStore) GetSpend(_ context.Context, key, day string) (float64, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, 0, m.err
	}
	k := key + "/" + day
	return m.usd[k], m.runs[k], nil
}

func (m *memStore) ListSpend(_ context.Context, day string) ([]Spend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var out []Spend
	for k, v := range m.usd {
		key, d, _ := strings.Cut(k, "/")
		if d == day {
			out = append(out, Spend{Key: key, Day: day, USD: v, Runs: m.runs[k]})
		}
	}
	return out, nil
}

type fakePager struct {
	mu    sync.Mutex
	pages []string
}

func (p *fakePager) Page(_ context.Context, subject, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pages = append(p.pages, subject+": "+detail)
}

func (p *fakePager) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pages)
}

func fixedClock(t time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	now := t
	return func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}, func(d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}
}

func TestKindCeilingStopsThatKindOnly(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	clock, _ := fixedClock(time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC))
	l := New(st, map[string]float64{"code_fix": 1.0, "report": 5.0, Global: 100}, quiet)
	l.Now = clock

	if err := l.Check(ctx, "code_fix"); err != nil {
		t.Fatalf("fresh ledger refused: %v", err)
	}
	if err := l.Record(ctx, "code_fix", 0.6); err != nil {
		t.Fatal(err)
	}
	if err := l.Check(ctx, "code_fix"); err != nil {
		t.Fatalf("under ceiling refused: %v", err)
	}
	if err := l.Record(ctx, "code_fix", 0.5); err != nil { // 1.1 >= 1.0
		t.Fatal(err)
	}
	err := l.Check(ctx, "code_fix")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhausted kind not refused: %v", err)
	}
	var ex *ExhaustedError
	if !errors.As(err, &ex) || ex.Key != "code_fix" || ex.IsGlobal || ex.Limit != 1.0 || ex.Spent < 1.0 {
		t.Fatalf("error detail %+v", ex)
	}
	if ex.ResetsAt.Format(time.RFC3339) != "2026-09-16T00:00:00Z" {
		t.Errorf("reset %s", ex.ResetsAt)
	}
	if !strings.Contains(err.Error(), "code_fix") || !strings.Contains(err.Error(), "1.1 of 1.00 USD") {
		t.Errorf("message %q", err.Error())
	}
	// A sub-cent ceiling must not be rounded away in the message an API client
	// sees. Its own store, so it does not disturb the totals asserted below.
	small := New(newMem(), map[string]float64{"tiny": 0.001}, quiet)
	small.Now = clock
	if e := small.Record(ctx, "tiny", 0.02); e != nil {
		t.Fatal(e)
	}
	if msg := small.Check(ctx, "tiny").Error(); !strings.Contains(msg, "of 0.001 USD") {
		t.Errorf("small ceiling rendered as %q", msg)
	}
	// Another kind is unaffected, and the global total includes both.
	if err := l.Check(ctx, "report"); err != nil {
		t.Errorf("unrelated kind refused: %v", err)
	}
	if g, _, _ := st.GetSpend(ctx, Global, Day(clock())); g != 1.1 {
		t.Errorf("global total %v", g)
	}
}

func TestGlobalCeilingOpensBreakerAndPagesOnce(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	clock, advance := fixedClock(time.Date(2026, 9, 15, 23, 0, 0, 0, time.UTC))
	pager := &fakePager{}
	l := New(st, map[string]float64{"code_fix": 100, Global: 1.0}, quiet)
	l.Now, l.Pager = clock, pager

	if err := l.Record(ctx, "code_fix", 1.5); err != nil {
		t.Fatal(err)
	}
	if !l.BreakerOpen() {
		t.Fatal("breaker did not open on the global breach")
	}
	// Every kind is refused, with the breaker flagged.
	for _, kind := range []string{"code_fix", "report"} {
		err := l.Check(ctx, kind)
		var ex *ExhaustedError
		if !errors.As(err, &ex) || !ex.IsGlobal || !ex.Breaker {
			t.Fatalf("kind %s: %v (%+v)", kind, err, ex)
		}
	}
	if l.AllowedKinds(ctx, []string{"code_fix", "report"}) == nil {
		t.Error("AllowedKinds must not report `any kind` while the breaker is open")
	}
	if got := l.AllowedKinds(ctx, []string{"code_fix", "report"}); len(got) != 0 {
		t.Errorf("AllowedKinds %v", got)
	}
	// Paged exactly once, however many runs record afterwards.
	if err := l.Record(ctx, "code_fix", 0.2); err != nil {
		t.Fatal(err)
	}
	if pager.count() != 1 {
		t.Errorf("pages %d, want 1", pager.count())
	}
	if !strings.Contains(pager.pages[0], "global daily budget exhausted") {
		t.Errorf("page %q", pager.pages[0])
	}

	// The ceiling is per UTC day: the breaker closes itself after midnight.
	advance(2 * time.Hour)
	if l.BreakerOpen() {
		t.Error("breaker still open on the next day")
	}
	if err := l.Check(ctx, "code_fix"); err != nil {
		t.Errorf("new day refused: %v", err)
	}
	// And it re-opens (and pages again) if the new day breaches too.
	if err := l.Record(ctx, "code_fix", 2); err != nil {
		t.Fatal(err)
	}
	if !l.BreakerOpen() || pager.count() != 2 {
		t.Errorf("breaker=%v pages=%d", l.BreakerOpen(), pager.count())
	}
	l.ResetBreaker()
	if l.BreakerOpen() {
		t.Error("ResetBreaker did not close it")
	}
}

func TestAllowedKindsFiltersSaturatedKinds(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	clock, _ := fixedClock(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC))
	l := New(st, map[string]float64{"code_fix": 1, "report": 1, Global: 100}, quiet)
	l.Now = clock

	if got := l.AllowedKinds(ctx, []string{"code_fix", "report", "triage"}); got != nil {
		t.Errorf("nothing saturated should mean `any kind` (nil), got %v", got)
	}
	if err := l.Record(ctx, "code_fix", 2); err != nil {
		t.Fatal(err)
	}
	got := l.AllowedKinds(ctx, []string{"code_fix", "report", "triage"})
	if strings.Join(got, ",") != "report,triage" {
		t.Errorf("AllowedKinds %v", got)
	}
	// A kind with no ceiling is never filtered.
	l2 := New(st, map[string]float64{}, quiet)
	l2.Now = clock
	if got := l2.AllowedKinds(ctx, []string{"code_fix"}); got != nil {
		t.Errorf("ledger without limits must not filter: %v", got)
	}
}

// A ledger read error must not stop all work: the per-run ceiling still bounds
// each worker, so the ledger fails open and logs.
func TestLedgerFailsOpenOnStoreError(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	st.err = errors.New("db down")
	l := New(st, map[string]float64{"code_fix": 1}, quiet)
	if err := l.Check(ctx, "code_fix"); err != nil {
		t.Errorf("ledger did not fail open: %v", err)
	}
	if err := l.Record(ctx, "code_fix", 1); err == nil {
		t.Error("Record must report the write failure")
	}
}

func TestNilLedgerAndReport(t *testing.T) {
	ctx := context.Background()
	var nilLedger *Ledger
	if err := nilLedger.Check(ctx, "code_fix"); err != nil {
		t.Errorf("nil ledger refused: %v", err)
	}
	if err := nilLedger.Record(ctx, "code_fix", 1); err != nil {
		t.Errorf("nil ledger record: %v", err)
	}
	if nilLedger.BreakerOpen() || nilLedger.AllowedKinds(ctx, []string{"x"}) != nil || nilLedger.Limit("x") != 0 {
		t.Error("nil ledger must be inert")
	}

	st := newMem()
	clock, _ := fixedClock(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC))
	l := New(st, map[string]float64{"code_fix": 10, Global: 20, "report": 5}, quiet)
	l.Now = clock
	if err := l.Record(ctx, "code_fix", 3); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("report rows %+v", rows)
	}
	byKey := map[string]Spend{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if cf := byKey["code_fix"]; cf.USD != 3 || cf.Limit != 10 || cf.Runs != 1 || cf.Remaining() != 7 || cf.Exceeded() {
		t.Errorf("code_fix %+v", cf)
	}
	if g := byKey[Global]; g.USD != 3 || g.Limit != 20 {
		t.Errorf("global %+v", g)
	}
	// A kind with a ceiling but no spend still appears, so dashboards show it.
	if r := byKey["report"]; r.Limit != 5 || r.USD != 0 {
		t.Errorf("report %+v", r)
	}
	if (Spend{USD: 5, Limit: 0}).Exceeded() {
		t.Error("a zero limit means unlimited")
	}
}

func TestConcurrentRecordIsSerialized(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	l := New(st, map[string]float64{"code_fix": 100, Global: 100}, quiet)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Record(ctx, "code_fix", 0.01); err != nil {
				t.Error(err)
			}
			_ = l.Check(ctx, "code_fix")
			_ = l.BreakerOpen()
		}()
	}
	wg.Wait()
	usd, runs, _ := st.GetSpend(ctx, "code_fix", Day(time.Now()))
	if runs != 50 || usd < 0.49 || usd > 0.51 {
		t.Errorf("usd=%v runs=%d", usd, runs)
	}
}

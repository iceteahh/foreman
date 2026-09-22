// Package budget enforces the three spend ceilings of design §8: per run
// (the CLI's --max-budget-usd plus the runner's token backstop, already in
// Step 5), per task kind per day, and globally per day. The per-day ceilings
// live in a Ledger: intake refuses new work for a saturated kind, the
// orchestrator stops leasing it, and a global breach opens a circuit breaker
// that also pages (plan Steps 15 and 18).
package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Global is the ledger key for the all-kinds ceiling.
const Global = "global"

// Spend is one day's recorded spend for a key (a kind, or Global).
type Spend struct {
	Key   string
	Day   string // UTC date, YYYY-MM-DD
	USD   float64
	Runs  int
	Limit float64
}

// Remaining is what is left of the ceiling (negative once breached).
func (s Spend) Remaining() float64 { return s.Limit - s.USD }

// Exceeded reports whether the ceiling is reached. A zero limit means no ceiling.
func (s Spend) Exceeded() bool { return s.Limit > 0 && s.USD >= s.Limit }

// Store persists daily spend. The SQLite store implements it; the interface
// keeps Postgres (plan Step 22) and tests out of the ledger's way.
type Store interface {
	// AddSpend adds usd to (key, day) and returns the new total.
	AddSpend(ctx context.Context, key, day string, usd float64, runs int) (float64, error)
	// GetSpend returns the recorded total for (key, day); 0 when absent.
	GetSpend(ctx context.Context, key, day string) (float64, int, error)
	// ListSpend returns every key's spend for a day, ordered by key.
	ListSpend(ctx context.Context, day string) ([]Spend, error)
}

// ErrExhausted is returned by Check when a ceiling is reached.
var ErrExhausted = errors.New("budget exhausted")

// ExhaustedError says which ceiling stopped the work.
type ExhaustedError struct {
	Key      string
	Day      string
	Spent    float64
	Limit    float64
	Kind     string // the task kind that was refused
	IsGlobal bool
	Breaker  bool // the global circuit breaker is open
	ResetsAt time.Time
}

func (e *ExhaustedError) Error() string {
	scope := "kind " + e.Key
	if e.IsGlobal {
		scope = "global"
	}
	msg := fmt.Sprintf("%s daily budget for %s reached: %s of %s USD spent on %s (resets %s)",
		ErrExhausted.Error(), scope, usd(e.Spent), usd(e.Limit), e.Day, e.ResetsAt.UTC().Format(time.RFC3339))
	if e.Breaker {
		msg += "; global circuit breaker open"
	}
	return msg
}

// usd formats a dollar amount without hiding a sub-cent ceiling behind
// two decimal places (a 0.001 limit must not print as 0.00).
func usd(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		return s + ".00"
	}
	return s
}

// Is makes errors.Is(err, ErrExhausted) work.
func (e *ExhaustedError) Is(target error) bool { return target == ErrExhausted }

// Pager is notified once when the global ceiling trips the circuit breaker
// (design §8 "circuit breaker + page"). Step 18 wires the Slack/PagerDuty sink.
type Pager interface {
	Page(ctx context.Context, subject, detail string)
}

// Ledger answers "may this kind run?" and records what runs cost.
type Ledger struct {
	Store Store
	// Limits are the daily ceilings in USD by kind, plus Global. A missing or
	// zero entry means unlimited.
	Limits map[string]float64
	Logger *slog.Logger
	// Now defaults to time.Now (tests inject a fake clock).
	Now func() time.Time
	// Pager receives the page when the global breaker opens.
	Pager Pager

	mu sync.Mutex
	// breakerDay is the day the global breaker is open for ("" when closed):
	// it re-closes by itself when the UTC day rolls over.
	breakerDay string
	paged      string
}

// New builds a ledger.
func New(store Store, limits map[string]float64, logger *slog.Logger) *Ledger {
	return &Ledger{Store: store, Limits: limits, Logger: logger}
}

func (l *Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *Ledger) log() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// Day is the ledger's UTC day key.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// nextMidnight is when the day's ceilings reset.
func nextMidnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}

// Limit returns the ceiling for a key (0 = unlimited).
func (l *Ledger) Limit(key string) float64 {
	if l == nil || l.Limits == nil {
		return 0
	}
	return l.Limits[key]
}

// Check reports whether a task of this kind may start now. It returns an
// *ExhaustedError (matching ErrExhausted) when the kind's or the global
// ceiling is reached. A nil ledger never refuses.
func (l *Ledger) Check(ctx context.Context, kind string) error {
	if l == nil || l.Store == nil {
		return nil
	}
	day := Day(l.now())
	if l.BreakerOpen() {
		spent, _, _ := l.Store.GetSpend(ctx, Global, day)
		return &ExhaustedError{Key: Global, Day: day, Spent: spent, Limit: l.Limit(Global), Kind: kind,
			IsGlobal: true, Breaker: true, ResetsAt: nextMidnight(l.now())}
	}
	for _, key := range []string{kind, Global} {
		limit := l.Limit(key)
		if limit <= 0 {
			continue
		}
		spent, _, err := l.Store.GetSpend(ctx, key, day)
		if err != nil {
			// Fail open on a ledger read error: refusing every task because the
			// bookkeeping is unavailable is worse than briefly overspending,
			// and the per-run ceiling still bounds each worker.
			l.log().Error("budget check failed; allowing the run", "key", key, "err", err)
			return nil
		}
		if spent >= limit {
			return &ExhaustedError{Key: key, Day: day, Spent: spent, Limit: limit, Kind: kind,
				IsGlobal: key == Global, ResetsAt: nextMidnight(l.now())}
		}
	}
	return nil
}

// Reservation is a claim held against the day's ceilings while a run executes.
// Settle it with what the run actually cost.
type Reservation struct {
	Kind string
	Day  string
	// USD is what was claimed (the run's max_cost_usd).
	USD float64
}

// Reserve claims usd against the kind's and the global ceiling before a run
// starts, and refuses when the claim would cross one.
//
// Check alone is a soft ceiling: it reads the spend so far, so N runs starting
// together all see room and all start, and the day's total ends up N max-costs
// over. Reserving the run's own ceiling up front makes the limit hard — the
// worst case is the budget looking fuller than it is while runs are in flight,
// which Settle corrects the moment each finishes.
//
// A nil ledger, a nil store or a zero claim reserves nothing and never refuses.
func (l *Ledger) Reserve(ctx context.Context, kind string, claim float64) (*Reservation, error) {
	if l == nil || l.Store == nil || claim <= 0 {
		return nil, nil
	}
	if err := l.Check(ctx, kind); err != nil {
		return nil, err
	}
	day := Day(l.now())
	// A run whose own ceiling is larger than the day's would never fit, even
	// on an empty ledger: the kind would silently never run. Say so plainly
	// rather than reporting it as today's budget being used up.
	for _, key := range []string{kind, Global} {
		if limit := l.Limit(key); limit > 0 && claim > limit {
			return nil, fmt.Errorf("%w: a run of kind %s may cost up to %s but the daily ceiling for %s is %s, so it can never start; raise budgets.daily_usd.%s or lower the kind's max_cost_usd",
				ErrExhausted, kind, usd(claim), key, usd(limit), key)
		}
	}
	res := &Reservation{Kind: kind, Day: day, USD: claim}
	// The claim lands on both ceilings; the run is counted once, here, so a
	// later Settle only moves money.
	claimed := make([]string, 0, 2)
	for _, key := range []string{kind, Global} {
		total, err := l.Store.AddSpend(ctx, key, day, claim, 1)
		if err != nil {
			// Fail open, as Check does: refusing every run because the
			// bookkeeping is unavailable is worse than briefly overspending,
			// and the per-run ceiling still bounds each worker.
			l.log().Error("budget reservation failed; allowing the run unreserved", "key", key, "err", err)
			l.rollback(ctx, day, claimed, claim)
			return nil, nil
		}
		claimed = append(claimed, key)
		limit := l.Limit(key)
		if limit > 0 && total > limit {
			// This claim is what crossed the line: hand it back and refuse.
			// Two replicas racing may both roll back and both be refused,
			// which over-refuses for one tick and never over-admits.
			l.rollback(ctx, day, claimed, claim)
			return nil, &ExhaustedError{Key: key, Day: day, Spent: total - claim, Limit: limit, Kind: kind,
				IsGlobal: key == Global, ResetsAt: nextMidnight(l.now())}
		}
	}
	return res, nil
}

// rollback returns a partial claim to the pool.
func (l *Ledger) rollback(ctx context.Context, day string, keys []string, claim float64) {
	for _, key := range keys {
		if _, err := l.Store.AddSpend(ctx, key, day, -claim, -1); err != nil {
			l.log().Error("returning a budget reservation failed; the day reads fuller than it is until midnight",
				"key", key, "day", day, "usd", claim, "err", err)
		}
	}
}

// Settle replaces a reservation with what the run actually cost. The
// difference is usually negative: a run rarely spends its whole ceiling, and
// until this runs that headroom is unavailable to everything else.
//
// Settling a nil reservation records the cost outright, which is what an
// unreserved caller (the judge) wants.
func (l *Ledger) Settle(ctx context.Context, res *Reservation, actualUSD float64) error {
	if l == nil || l.Store == nil {
		return nil
	}
	if res == nil {
		return l.Record(ctx, "", actualUSD)
	}
	delta := actualUSD - res.USD
	var errs []error
	for _, key := range []string{res.Kind, Global} {
		// runs 0: the reservation already counted this run.
		total, err := l.Store.AddSpend(ctx, key, res.Day, delta, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("settle spend for %s: %w", key, err))
			continue
		}
		limit := l.Limit(key)
		if limit > 0 && total >= limit {
			if key == Global {
				l.openBreaker(ctx, res.Day, total, limit)
			} else {
				l.log().Warn("daily budget for kind reached; the orchestrator stops leasing it",
					"kind", key, "day", res.Day, "spent_usd", total, "limit_usd", limit)
			}
		}
	}
	return errors.Join(errs...)
}

// Record adds a finished run's cost to the kind and global totals. It opens
// the circuit breaker (and pages once) when the global ceiling is crossed.
// Reserve/Settle is the hard-ceiling path; Record is for spend nobody reserved
// (the judge), where the amount is small and only known afterwards.
func (l *Ledger) Record(ctx context.Context, kind string, usd float64) error {
	if l == nil || l.Store == nil {
		return nil
	}
	day := Day(l.now())
	keys := []string{kind, Global}
	if kind == "" || kind == Global {
		keys = []string{Global}
	}
	var errs []error
	for _, key := range keys {
		total, err := l.Store.AddSpend(ctx, key, day, usd, 1)
		if err != nil {
			errs = append(errs, fmt.Errorf("record spend for %s: %w", key, err))
			continue
		}
		limit := l.Limit(key)
		if limit > 0 && total >= limit {
			if key == Global {
				l.openBreaker(ctx, day, total, limit)
			} else {
				l.log().Warn("daily budget for kind reached; the orchestrator stops leasing it",
					"kind", key, "day", day, "spent_usd", total, "limit_usd", limit)
			}
		}
	}
	return errors.Join(errs...)
}

// openBreaker latches the global breaker for the day and pages once.
func (l *Ledger) openBreaker(ctx context.Context, day string, spent, limit float64) {
	l.mu.Lock()
	first := l.breakerDay != day
	l.breakerDay = day
	page := l.paged != day
	if page {
		l.paged = day
	}
	l.mu.Unlock()
	if first {
		l.log().Error("global daily budget reached; circuit breaker open, no new runs will start",
			"day", day, "spent_usd", spent, "limit_usd", limit, "resets_at", nextMidnight(l.now()))
	}
	if page && l.Pager != nil {
		l.Pager.Page(ctx, "harness: global daily budget exhausted",
			fmt.Sprintf("Spent %.4f of %.2f USD on %s. No new runs start until %s. Raise budgets.daily_usd.global or wait for the reset.",
				spent, limit, day, nextMidnight(l.now()).Format(time.RFC3339)))
	}
}

// BreakerOpen reports whether the global breaker is open for the current day.
// It closes by itself when the UTC day rolls over.
func (l *Ledger) BreakerOpen() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.breakerDay != "" && l.breakerDay == Day(l.now())
}

// ResetBreaker closes the breaker (operator override after raising the ceiling).
func (l *Ledger) ResetBreaker() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.breakerDay = ""
	l.mu.Unlock()
}

// AllowedKinds filters kinds down to those still inside their ceiling. It
// returns nil when every kind is allowed (the queue's "any kind" case) and an
// empty slice when nothing may run.
func (l *Ledger) AllowedKinds(ctx context.Context, kinds []string) []string {
	if l == nil || l.Store == nil || len(l.Limits) == 0 {
		return nil
	}
	if l.BreakerOpen() {
		return []string{}
	}
	day := Day(l.now())
	if limit := l.Limit(Global); limit > 0 {
		if spent, _, err := l.Store.GetSpend(ctx, Global, day); err == nil && spent >= limit {
			return []string{}
		}
	}
	blocked := false
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		limit := l.Limit(k)
		if limit > 0 {
			if spent, _, err := l.Store.GetSpend(ctx, k, day); err == nil && spent >= limit {
				blocked = true
				continue
			}
		}
		out = append(out, k)
	}
	if !blocked {
		return nil
	}
	sort.Strings(out)
	return out
}

// Report is today's spend for every key with a ceiling or recorded spend.
func (l *Ledger) Report(ctx context.Context) ([]Spend, error) {
	if l == nil || l.Store == nil {
		return nil, nil
	}
	day := Day(l.now())
	rows, err := l.Store.ListSpend(ctx, day)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range rows {
		rows[i].Limit = l.Limit(rows[i].Key)
		seen[rows[i].Key] = true
	}
	for key, limit := range l.Limits {
		if !seen[key] {
			rows = append(rows, Spend{Key: key, Day: day, Limit: limit})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows, nil
}

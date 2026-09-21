package golden

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

// Executor runs one prepared task spec through the real pipeline and reports
// what happened. It is the seam between the suite and the orchestrator: the
// `harness` binary implements it over the live pool, tests implement it with
// canned executions, and neither knows about the other.
type Executor interface {
	Execute(ctx context.Context, spec task.Spec) (*Execution, error)
}

// Runner executes a suite and produces a Report.
type Runner struct {
	Suite    Suite
	Executor Executor
	// WorkRoot is where fixture origins are materialised (a temp dir by default).
	WorkRoot string
	Git      string
	// MaxCostUSD stops the suite once the accumulated spend reaches it; the
	// report is marked BudgetExhausted and the remaining cases are skipped.
	// This is what makes a nightly CI run safe to schedule.
	MaxCostUSD float64
	// CaseTimeout bounds one case (default 30 minutes).
	CaseTimeout time.Duration
	// Select, when non-empty, restricts the run to these case ids.
	Select []string
	// Tags, when non-empty, restricts the run to cases carrying any of them.
	Tags []string
	// KeepFixtures leaves the materialised origins on disk for debugging.
	KeepFixtures bool
	// Repeat runs each case this many times and scores the majority verdict
	// (default 1). A worker is not a deterministic function: two runs of an
	// unchanged harness on 2026-09-21 flipped 4 of 21 cases, so a single
	// sample cannot distinguish a flaky case from a real regression. Cost
	// scales with Repeat, and the cost cap still applies between samples.
	Repeat int
	// OnCase is called after each case is scored (progress output).
	OnCase func(CaseResult)
	Logger *slog.Logger
	Now    func() time.Time

	// Report metadata, copied through to the output.
	HarnessVersion string
	CLIVersion     string
	WorkerMode     string
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run executes every selected case in order and returns the scored report.
// It returns a report even on error, so a suite interrupted halfway is still
// inspectable.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	if r.Executor == nil {
		return nil, errors.New("golden: no executor")
	}
	work := r.WorkRoot
	if work == "" {
		d, err := os.MkdirTemp("", "golden-")
		if err != nil {
			return nil, err
		}
		work = d
		if !r.KeepFixtures {
			defer func() { _ = os.RemoveAll(d) }()
		}
	}
	fixtures := &Fixtures{Root: work, Git: r.Git}

	rep := &Report{
		Suite: r.Suite.Dir, StartedAt: r.now(), MaxCostUSD: r.MaxCostUSD,
		HarnessVersion: r.HarnessVersion, CLIVersion: r.CLIVersion, WorkerMode: r.WorkerMode,
	}
	var spent float64
	for _, c := range r.Suite.Cases {
		if err := ctx.Err(); err != nil {
			rep.Cases = append(rep.Cases, skipped(c, "suite cancelled"))
			continue
		}
		if why, skip := r.skipReason(c, spent); skip {
			if strings.HasPrefix(why, "cost cap") {
				rep.BudgetExhausted = true
			}
			rep.Cases = append(rep.Cases, skipped(c, why))
			continue
		}
		res := r.runSamples(ctx, fixtures, c, &spent)
		rep.Cases = append(rep.Cases, res)
		if r.OnCase != nil {
			r.OnCase(res)
		}
	}
	rep.FinishedAt = r.now()
	rep.Summarize()
	return rep, nil
}

// runSamples runs one case Repeat times and reduces the samples to a single
// result whose OK is the majority verdict. It stops early when the cost cap is
// reached, so a partial sample set never silently becomes a full one: the
// surviving samples are what Passes/Samples report. spent is updated as it
// goes, because each sample costs money whether or not the case passes.
func (r *Runner) runSamples(ctx context.Context, fixtures *Fixtures, c *Case, spent *float64) CaseResult {
	n := r.Repeat
	if n < 1 {
		n = 1
	}
	first := r.runCase(ctx, fixtures, c)
	*spent += first.CostUSD
	if n == 1 {
		return first
	}
	passes := 0
	if first.OK {
		passes++
	}
	worst, samples := first, 1
	for i := 1; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		if _, capped := r.skipReason(c, *spent); capped {
			break
		}
		s := r.runCase(ctx, fixtures, c)
		*spent += s.CostUSD
		samples++
		if s.OK {
			passes++
		} else if worst.OK {
			// Keep a failing sample's evidence: a majority-pass case still has
			// to show why it failed the times it did.
			reasons, cost, dur := worst.Reasons, worst.CostUSD, worst.DurationMS
			worst = s
			worst.Reasons = append(reasons, s.Reasons...)
			worst.CostUSD, worst.DurationMS = cost, dur
		}
		worst.CostUSD += s.CostUSD
		worst.DurationMS += s.DurationMS
	}
	out := worst
	out.Samples, out.Passes = samples, passes
	out.OK = passes*2 > samples
	return out
}

// skipReason decides whether a case runs at all, and says why not.
func (r *Runner) skipReason(c *Case, spent float64) (string, bool) {
	if c.Skip != "" {
		return c.Skip, true
	}
	if len(r.Select) > 0 && !contains(r.Select, c.ID) {
		return "not selected", true
	}
	if len(r.Tags) > 0 && !hasAnyTag(c, r.Tags) {
		return "no matching tag", true
	}
	if r.MaxCostUSD > 0 && spent >= r.MaxCostUSD {
		return fmt.Sprintf("cost cap reached ($%.2f of $%.2f spent)", spent, r.MaxCostUSD), true
	}
	return "", false
}

// runCase materialises the fixture, executes the case and scores it. A failure
// to materialise is the suite's own fault, not the harness's, so it is
// reported as an error row rather than aborting the run.
func (r *Runner) runCase(ctx context.Context, fixtures *Fixtures, c *Case) CaseResult {
	log := r.log().With("case", c.ID, "kind", string(c.Kind))
	start := r.now()
	origin, err := fixtures.Materialize(ctx, c)
	if err != nil {
		log.Error("fixture failed", "err", err)
		return Score(c, &Execution{Err: fmt.Errorf("fixture: %w", err), DurationMS: r.elapsed(start)})
	}
	if !r.KeepFixtures {
		defer func() { _ = os.RemoveAll(origin) }()
	}

	cctx := ctx
	if d := r.caseTimeout(); d > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	log.Info("case started", "repo", origin)
	ex, err := r.Executor.Execute(cctx, c.Spec(origin))
	if ex == nil {
		ex = &Execution{}
	}
	if err != nil && ex.Err == nil {
		ex.Err = err
	}
	if ex.DurationMS == 0 {
		ex.DurationMS = r.elapsed(start)
	}
	res := Score(c, ex)
	log.Info("case scored", "ok", res.OK, "expected", string(res.Expected), "actual", string(res.Actual),
		"attempts", res.Attempts, "cost_usd", res.CostUSD, "reasons", res.Reasons)
	return res
}

func (r *Runner) caseTimeout() time.Duration {
	if r.CaseTimeout > 0 {
		return r.CaseTimeout
	}
	return 30 * time.Minute
}

func (r *Runner) elapsed(start time.Time) int64 {
	return r.now().Sub(start).Milliseconds()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func hasAnyTag(c *Case, tags []string) bool {
	for _, t := range tags {
		if c.HasTag(t) {
			return true
		}
	}
	return false
}

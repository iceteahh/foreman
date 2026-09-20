package obs

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Metrics is the design §8 metric set. Every instrument is safe to call on a
// zero value obtained from noopMetrics(), so call sites never branch.
type Metrics struct {
	// runs by status (and kind): counter incremented on every terminal state.
	runs metric.Int64Counter
	// runDuration seconds, by kind and status: p50/p95 come from the histogram.
	runDuration metric.Float64Histogram
	// turns per run.
	turns metric.Int64Histogram
	// cost per run, and the daily-spend gauge the ledger feeds.
	cost       metric.Float64Histogram
	spend      metric.Float64Counter
	spendDay   metric.Float64ObservableGauge
	spendLimit metric.Float64ObservableGauge
	spendFunc  func() []SpendSample
	// permissionDenials and retries: rates over these counters.
	permissionDenials metric.Int64Counter
	retries           metric.Int64Counter
	// judge verdicts and human overrides: disagreement rate.
	judgeVerdicts  metric.Int64Counter
	humanDecisions metric.Int64Counter
	disagreements  metric.Int64Counter
	// queue depth and age, reported by an observable callback.
	queueDepth metric.Int64ObservableGauge
	queueAge   metric.Float64ObservableGauge
	queueFunc  func() QueueSample
	// phase timings inside a run (worker, checks, judge, deliver).
	phaseDuration metric.Float64Histogram
	// rate limiting and dead letters.
	rateLimited metric.Int64Counter
	deadLetters metric.Int64Counter
	// fan-out workflows (plan Step 21).
	fanOuts  metric.Int64Counter
	children metric.Int64Counter
	fanIns   metric.Int64Counter
	// workers currently running.
	active     metric.Int64UpDownCounter
	deliveries metric.Int64Counter
}

// QueueSample is what the queue reports to the observable gauges.
type QueueSample struct {
	Ready, Leased, Dead int
	OldestReadyAge      time.Duration
}

// SpendSample is one key's spend against its ceiling.
type SpendSample struct {
	Key   string
	USD   float64
	Limit float64
}

func noopMetrics() *Metrics {
	m, err := NewMetrics(noop.NewMeterProvider().Meter(ServiceName))
	if err != nil {
		// The no-op meter cannot fail; a zero Metrics would panic on use.
		panic(err)
	}
	return m
}

// NewMetrics registers every instrument on the meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{}
	var err error
	if m.runs, err = meter.Int64Counter("harness.runs",
		metric.WithDescription("Runs reaching a terminal status, by kind and status")); err != nil {
		return nil, wrap(err)
	}
	if m.runDuration, err = meter.Float64Histogram("harness.run.duration",
		metric.WithDescription("Wall-clock seconds per run"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 5, 15, 30, 60, 120, 300, 600, 900, 1800)); err != nil {
		return nil, wrap(err)
	}
	if m.turns, err = meter.Int64Histogram("harness.run.turns",
		metric.WithDescription("Assistant turns per run"),
		metric.WithExplicitBucketBoundaries(1, 2, 3, 5, 8, 13, 21, 30, 50)); err != nil {
		return nil, wrap(err)
	}
	if m.cost, err = meter.Float64Histogram("harness.run.cost",
		metric.WithDescription("USD per run (result.total_cost_usd)"), metric.WithUnit("USD"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)); err != nil {
		return nil, wrap(err)
	}
	if m.spend, err = meter.Float64Counter("harness.spend",
		metric.WithDescription("Cumulative USD spent, by kind"), metric.WithUnit("USD")); err != nil {
		return nil, wrap(err)
	}
	if m.permissionDenials, err = meter.Int64Counter("harness.permission_denials",
		metric.WithDescription("Tool calls denied by the permission policy, by tool")); err != nil {
		return nil, wrap(err)
	}
	if m.retries, err = meter.Int64Counter("harness.retries",
		metric.WithDescription("Retries scheduled, by kind and session mode")); err != nil {
		return nil, wrap(err)
	}
	if m.judgeVerdicts, err = meter.Int64Counter("harness.judge.verdicts",
		metric.WithDescription("Judge verdicts, by verdict and whether checks were gamed")); err != nil {
		return nil, wrap(err)
	}
	if m.humanDecisions, err = meter.Int64Counter("harness.review.decisions",
		metric.WithDescription("Human review decisions, by action and source")); err != nil {
		return nil, wrap(err)
	}
	if m.disagreements, err = meter.Int64Counter("harness.review.disagreements",
		metric.WithDescription("Human decisions that contradicted the judge, by judge verdict and action")); err != nil {
		return nil, wrap(err)
	}
	if m.phaseDuration, err = meter.Float64Histogram("harness.stage.duration",
		metric.WithDescription("Seconds per pipeline stage (worker, checks, judge, deliver)"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 5, 15, 60, 300, 900)); err != nil {
		return nil, wrap(err)
	}
	if m.rateLimited, err = meter.Int64Counter("harness.rate_limited",
		metric.WithDescription("Runs that saw an API rate limit")); err != nil {
		return nil, wrap(err)
	}
	if m.deadLetters, err = meter.Int64Counter("harness.dead_letters",
		metric.WithDescription("Runs dead-lettered, by kind")); err != nil {
		return nil, wrap(err)
	}
	if m.fanOuts, err = meter.Int64Counter("harness.fanout.plans",
		metric.WithDescription("Planner runs decomposed into child tasks, by kind")); err != nil {
		return nil, wrap(err)
	}
	if m.children, err = meter.Int64Counter("harness.fanout.children",
		metric.WithDescription("Fan-out child tasks created, by parent kind")); err != nil {
		return nil, wrap(err)
	}
	if m.fanIns, err = meter.Int64Counter("harness.fanout.fan_ins",
		metric.WithDescription("Synthesizer runs queued once every child finished, by kind and whether every child delivered")); err != nil {
		return nil, wrap(err)
	}
	if m.active, err = meter.Int64UpDownCounter("harness.workers.active",
		metric.WithDescription("Workers currently running, by kind")); err != nil {
		return nil, wrap(err)
	}
	if m.deliveries, err = meter.Int64Counter("harness.deliveries",
		metric.WithDescription("Delivery attempts, by adapter and result")); err != nil {
		return nil, wrap(err)
	}
	if m.queueDepth, err = meter.Int64ObservableGauge("harness.queue.depth",
		metric.WithDescription("Jobs in the queue, by state")); err != nil {
		return nil, wrap(err)
	}
	if m.queueAge, err = meter.Float64ObservableGauge("harness.queue.oldest_ready_age",
		metric.WithDescription("Age of the oldest ready job"), metric.WithUnit("s")); err != nil {
		return nil, wrap(err)
	}
	if m.spendDay, err = meter.Float64ObservableGauge("harness.budget.spend_today",
		metric.WithDescription("USD spent today, by ledger key (a task kind, or global)"), metric.WithUnit("USD")); err != nil {
		return nil, wrap(err)
	}
	// The ceiling is its own gauge, not a label on the spend gauge: alerting
	// rules and dashboards compare two series, which a label cannot express.
	if m.spendLimit, err = meter.Float64ObservableGauge("harness.budget.limit",
		metric.WithDescription("Configured daily ceiling in USD, by ledger key (0 = unlimited)"), metric.WithUnit("USD")); err != nil {
		return nil, wrap(err)
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if m.queueFunc != nil {
			s := m.queueFunc()
			o.ObserveInt64(m.queueDepth, int64(s.Ready), metric.WithAttributes(attribute.String("state", "ready")))
			o.ObserveInt64(m.queueDepth, int64(s.Leased), metric.WithAttributes(attribute.String("state", "leased")))
			o.ObserveInt64(m.queueDepth, int64(s.Dead), metric.WithAttributes(attribute.String("state", "dead")))
			o.ObserveFloat64(m.queueAge, s.OldestReadyAge.Seconds())
		}
		if m.spendFunc != nil {
			for _, s := range m.spendFunc() {
				attrs := metric.WithAttributes(attribute.String("key", s.Key))
				o.ObserveFloat64(m.spendDay, s.USD, attrs)
				o.ObserveFloat64(m.spendLimit, s.Limit, attrs)
			}
		}
		return nil
	}, m.queueDepth, m.queueAge, m.spendDay, m.spendLimit)
	if err != nil {
		return nil, wrap(err)
	}
	return m, nil
}

func wrap(err error) error { return fmt.Errorf("obs metrics: %w", err) }

// ObserveQueue registers the callback the queue gauges read. Call it once.
func (m *Metrics) ObserveQueue(f func() QueueSample) {
	if m != nil {
		m.queueFunc = f
	}
}

// ObserveSpend registers the callback the budget gauge reads. Call it once.
func (m *Metrics) ObserveSpend(f func() []SpendSample) {
	if m != nil {
		m.spendFunc = f
	}
}

// RunFinished records a run reaching a terminal status.
func (m *Metrics) RunFinished(ctx context.Context, kind, status, terminalReason string, d time.Duration, turns int, costUSD float64) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("kind", kind), attribute.String("status", status),
		attribute.String("terminal_reason", terminalReason))
	m.runs.Add(ctx, 1, attrs)
	if d > 0 {
		m.runDuration.Record(ctx, d.Seconds(), attrs)
	}
	if turns > 0 {
		m.turns.Record(ctx, int64(turns), metric.WithAttributes(attribute.String("kind", kind)))
	}
	if costUSD > 0 {
		kindAttr := metric.WithAttributes(attribute.String("kind", kind))
		m.cost.Record(ctx, costUSD, kindAttr)
		m.spend.Add(ctx, costUSD, kindAttr)
	}
}

// Stage records how long one pipeline stage took.
func (m *Metrics) Stage(ctx context.Context, kind, stage string, d time.Duration) {
	if m == nil {
		return
	}
	m.phaseDuration.Record(ctx, d.Seconds(),
		metric.WithAttributes(attribute.String("kind", kind), attribute.String("stage", stage)))
}

// PermissionDenied records one denial.
func (m *Metrics) PermissionDenied(ctx context.Context, kind, tool string) {
	if m == nil {
		return
	}
	m.permissionDenials.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("tool", tool)))
}

// Retry records a scheduled retry.
func (m *Metrics) Retry(ctx context.Context, kind, sessionMode string) {
	if m == nil {
		return
	}
	m.retries.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("session_mode", sessionMode)))
}

// JudgeVerdict records a Layer 2 verdict.
func (m *Metrics) JudgeVerdict(ctx context.Context, kind, verdict string, gamed bool, costUSD float64) {
	if m == nil {
		return
	}
	m.judgeVerdicts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
		attribute.String("verdict", verdict), attribute.Bool("gamed_checks", gamed)))
	if costUSD > 0 {
		m.spend.Add(ctx, costUSD, metric.WithAttributes(attribute.String("kind", "judge")))
	}
}

// ReviewDecision records a human decision and, when it contradicts the judge,
// the disagreement that design §8 tracks (approving a fail, rejecting a pass).
func (m *Metrics) ReviewDecision(ctx context.Context, kind, action, source, judgeVerdict string) {
	if m == nil {
		return
	}
	m.humanDecisions.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
		attribute.String("action", action), attribute.String("source", source)))
	if Disagrees(judgeVerdict, action) {
		m.disagreements.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
			attribute.String("judge_verdict", judgeVerdict), attribute.String("action", action)))
	}
}

// Disagrees reports whether a human decision contradicted the judge: approving
// what the judge failed, or rejecting/closing what it passed. An `uncertain`
// verdict is what review exists for, so it is never a disagreement.
func Disagrees(judgeVerdict, action string) bool {
	switch judgeVerdict {
	case "fail":
		return action == "approve"
	case "pass":
		return action == "reject" || action == "close"
	}
	return false
}

// RateLimited records an API rate limit.
func (m *Metrics) RateLimited(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.rateLimited.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// DeadLetter records a dead-lettered run.
func (m *Metrics) DeadLetter(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.deadLetters.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// FanOut records one planner decomposition and the children it produced.
func (m *Metrics) FanOut(ctx context.Context, kind string, children int) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("kind", kind))
	m.fanOuts.Add(ctx, 1, attrs)
	m.children.Add(ctx, int64(children), attrs)
}

// FanIn records a synthesizer queued after every child finished. `complete`
// separates a fan-in where every child delivered from one that is merging over
// a hole — the second is the case worth alerting on.
func (m *Metrics) FanIn(ctx context.Context, kind string, children, delivered int) {
	if m == nil {
		return
	}
	m.fanIns.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
		attribute.Bool("complete", children > 0 && delivered == children)))
}

// WorkerStarted / WorkerStopped track the in-flight gauge.
func (m *Metrics) WorkerStarted(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.active.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// WorkerStopped decrements the in-flight gauge.
func (m *Metrics) WorkerStopped(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.active.Add(ctx, -1, metric.WithAttributes(attribute.String("kind", kind)))
}

// Delivered records a delivery attempt.
func (m *Metrics) Delivered(ctx context.Context, kind, adapter string, ok bool) {
	if m == nil {
		return
	}
	m.deliveries.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind),
		attribute.String("adapter", adapter), attribute.Bool("ok", ok)))
}

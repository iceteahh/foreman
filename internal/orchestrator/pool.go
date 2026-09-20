package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/100xteam-ai/foreman/internal/budget"
	"github.com/100xteam-ai/foreman/internal/deadletter"
	"github.com/100xteam-ai/foreman/internal/deliver"
	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/eval/judge"
	"github.com/100xteam-ai/foreman/internal/eval/route"
	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/obs"
	"github.com/100xteam-ai/foreman/internal/queue"
	"github.com/100xteam-ai/foreman/internal/review"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// PoolConfig bounds the pool.
type PoolConfig struct {
	Global  int
	PerKind map[string]int
	// PollInterval is the idle sleep between empty leases.
	PollInterval time.Duration
	// Lease is the initial lease; it is extended by heartbeat while a job runs.
	Lease time.Duration
	// MaxJobAttempts dead-letters a job that keeps failing before evaluation
	// (provision errors, api_error results).
	MaxJobAttempts int
	// APIErrorBackoff delays the requeue after an api_error result.
	APIErrorBackoff time.Duration
	// ConfigDirRoot holds per-run CLAUDE_CONFIG_DIRs.
	ConfigDirRoot string
	// KeepWorkspaces skips Destroy (debugging).
	KeepWorkspaces bool
	// CommandTimeout bounds each acceptance command.
	CommandTimeout time.Duration
	// ShutdownGrace is how long in-flight runs may continue after Run's ctx ends.
	ShutdownGrace time.Duration
	// DefaultThreshold is the judge score floor when policy.judge.threshold is 0.
	DefaultThreshold int
	// MaxBackoff caps the exponential requeue delay after rate limiting
	// (default 15 minutes).
	MaxBackoff time.Duration
	// MaxChildren caps one fan-out regardless of what the planner asked for
	// and what the kind's child template allows (0 = the template's cap).
	MaxChildren int
}

// Pool leases jobs and drives each run through worker → checks → judge →
// route → deliver / retry / review (design §5). It also implements
// review.Decider so human decisions re-enter the same lifecycle code.
type Pool struct {
	Store      store.Store
	Queue      queue.Queue
	Runner     *runner.Runner
	Workspaces workspace.Manager
	Checks     []checks.Check
	// Judge is Layer 2; nil means every judge-enabled policy yields `uncertain`
	// (→ human review) rather than silently passing.
	Judge *judge.Judge
	// Deliver ships passed runs; nil leaves them `passed`.
	Deliver deliver.Adapter
	// Submit creates fan-out child tasks (plan Step 21); nil disables fan-out,
	// which sends a planner run to human review rather than dropping its plan.
	Submit *Submitter
	// Review posts needs_review runs; nil falls back to review.LogChannel.
	Review review.Channel
	// Reviews tracks posts and decisions; nil disables SLA escalation and the decisions log.
	Reviews review.Store
	// Budget stops leasing kinds whose daily ceiling is reached and records
	// what every run cost (design §8); nil disables both.
	Budget *budget.Ledger
	// Dead records exhausted runs for operators and pages (plan Step 18);
	// nil keeps the run `dead` without a DLQ row.
	Dead deadletter.Sink
	// DeadStore is the dead-letter table `harness requeue` updates; nil skips
	// the bookkeeping (the requeue itself still works).
	DeadStore deadletter.Store
	// Obs carries metrics and the per-run trace; nil-safe, use obs.Disabled()
	// when observability is off.
	Obs *obs.Provider
	// Progress receives the live run timeline (Slack thread); nil disables it.
	Progress *obs.Progress
	Config   PoolConfig
	Logger   *slog.Logger
	Now      func() time.Time

	// Hooks for tests and metrics (Step 16).
	OnRunFinished func(t *task.Task, r *task.Run)

	kindSem map[string]chan struct{}
	semOnce sync.Once

	shedMu    sync.Mutex
	shedUntil time.Time

	obsOnce     sync.Once
	obsDisabled *obs.Provider
}

func (p *Pool) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

func (p *Pool) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// observe returns the provider, never nil.
func (p *Pool) observe() *obs.Provider {
	if p.Obs != nil {
		return p.Obs
	}
	p.obsOnce.Do(func() { p.obsDisabled = obs.Disabled() })
	return p.obsDisabled
}

func (p *Pool) metrics() *obs.Metrics { return p.observe().Metrics }

func (p *Pool) review() review.Channel {
	if p.Review != nil {
		return p.Review
	}
	return review.LogChannel{Logger: p.Logger}
}

func (p *Pool) init() {
	p.semOnce.Do(func() {
		p.kindSem = map[string]chan struct{}{}
		for k, n := range p.Config.PerKind {
			if n > 0 {
				p.kindSem[k] = make(chan struct{}, n)
			}
		}
		if p.Config.Global <= 0 {
			p.Config.Global = 1
		}
		if p.Config.PollInterval <= 0 {
			p.Config.PollInterval = time.Second
		}
		if p.Config.Lease <= 0 {
			p.Config.Lease = 2 * time.Minute
		}
		if p.Config.MaxJobAttempts <= 0 {
			p.Config.MaxJobAttempts = 5
		}
		if p.Config.APIErrorBackoff <= 0 {
			p.Config.APIErrorBackoff = time.Minute
		}
		if p.Config.ShutdownGrace <= 0 {
			p.Config.ShutdownGrace = 20 * time.Minute
		}
		if p.Config.DefaultThreshold <= 0 {
			p.Config.DefaultThreshold = 7
		}
		if p.Checks == nil {
			p.Checks = checks.Default()
		}
	})
}

// Run blocks until ctx is cancelled and every in-flight run has finished
// (or ShutdownGrace elapsed).
func (p *Pool) Run(ctx context.Context) error {
	p.init()
	drainCtx, drainCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer drainCancel()
	go func() {
		<-ctx.Done()
		t := time.AfterFunc(p.Config.ShutdownGrace, drainCancel)
		<-drainCtx.Done()
		t.Stop()
	}()
	var wg sync.WaitGroup
	for i := 0; i < p.Config.Global; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.worker(ctx, drainCtx, id)
		}(i)
	}
	wg.Wait()
	return nil
}

func (p *Pool) worker(ctx, drainCtx context.Context, id int) {
	log := p.log().With("worker", id)
	for {
		if ctx.Err() != nil {
			return
		}
		processed, err := p.ProcessOne(ctx, drainCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("process job", "err", err)
		}
		if !processed {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.Config.PollInterval):
			}
		}
	}
}

// ProcessOne leases and processes at most one job. It returns false when the
// queue had nothing for the kinds with free capacity. runCtx governs the
// in-flight run (may outlive leaseCtx during shutdown).
func (p *Pool) ProcessOne(leaseCtx, runCtx context.Context) (bool, error) {
	p.init()
	if paused, until := p.shedding(); paused {
		p.log().Debug("leasing paused by rate-limit shed", "until", until)
		return false, nil
	}
	kinds := p.leasableKinds(leaseCtx)
	if kinds != nil && len(kinds) == 0 {
		return false, nil // every kind is at its concurrency cap or out of budget
	}
	job, err := p.Queue.Lease(leaseCtx, kinds, p.Config.Lease)
	if errors.Is(err, queue.ErrEmpty) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	release := p.acquireKind(job.Kind)
	if release == nil {
		// Raced with another worker for the last slot: hand the job back.
		return false, p.Queue.Nack(leaseCtx, job, 0)
	}
	defer release()
	return true, p.process(runCtx, job)
}

// leasableKinds returns nil (any kind) when nothing is saturated, otherwise
// every known kind that still has both concurrency capacity and daily budget.
// An empty (non-nil) slice means nothing may be leased right now.
func (p *Pool) leasableKinds(ctx context.Context) []string {
	saturated := map[string]bool{}
	for k, sem := range p.kindSem {
		if len(sem) >= cap(sem) {
			saturated[k] = true
		}
	}
	all := p.knownKinds()
	budgeted := p.Budget.AllowedKinds(ctx, all)
	if budgeted != nil {
		if len(budgeted) == 0 {
			return []string{} // global ceiling or breaker: lease nothing
		}
		allowed := map[string]bool{}
		for _, k := range budgeted {
			allowed[k] = true
		}
		for _, k := range all {
			if !allowed[k] {
				saturated[k] = true
			}
		}
	}
	if len(saturated) == 0 {
		return nil
	}
	out := make([]string, 0, len(all))
	for _, k := range all {
		if !saturated[k] {
			out = append(out, k)
		}
	}
	return out
}

// knownKinds is every kind the pool might lease: the built-in kinds plus any
// kind that has its own concurrency cap.
func (p *Pool) knownKinds() []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, k := range task.Kinds {
		add(string(k))
	}
	for k := range p.kindSem {
		add(k)
	}
	return out
}

func (p *Pool) acquireKind(kind string) func() {
	sem, ok := p.kindSem[kind]
	if !ok {
		return func() {}
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }
	default:
		return nil
	}
}

// heartbeat extends the lease until stop is closed.
func (p *Pool) heartbeat(ctx context.Context, job *queue.Job, stop <-chan struct{}) {
	interval := p.Config.Lease / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := p.Queue.Extend(ctx, job, p.Config.Lease); err != nil {
				p.log().Warn("lease extend failed", "run_id", job.RunID, "err", err)
			}
		}
	}
}

// process drives one job end to end. Every exit path either Acks, Nacks or
// dead-letters the job.
func (p *Pool) process(ctx context.Context, job *queue.Job) (err error) {
	log := p.log().With("run_id", job.RunID, "task_id", job.TaskID, "job_attempt", job.Attempts)
	hbStop := make(chan struct{})
	go p.heartbeat(context.WithoutCancel(ctx), job, hbStop)
	defer close(hbStop)
	// Store writes must survive a cancelled run context.
	sctx := context.WithoutCancel(ctx)

	r, err := p.Store.GetRun(sctx, job.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Error("job references unknown run; dead-lettering")
			return p.Queue.DeadLetter(sctx, job, "run not found")
		}
		return err
	}
	t, err := p.Store.GetTask(sctx, r.TaskID)
	if err != nil {
		return err
	}
	if r.Status != task.StatusQueued {
		log.Warn("run not queued; acking stale job", "status", r.Status)
		return p.Queue.Ack(sctx, job)
	}
	if err := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusRunning, ""); err != nil {
		return err
	}
	if r.SessionMode == task.SessionNew || r.SessionMode == task.SessionFork {
		if err := p.Store.UpdateTaskSession(sctx, t.ID, r.SessionID); err != nil {
			return err
		}
	}
	// The effective task carries the phase's policy/acceptance and the run's prompt.
	et := task.Effective(t, r)

	// One trace per run: intake → queue → worker → checks → judge → route →
	// deliver (design §8). The queue wait is already known, so it is recorded
	// as the first child span.
	ctx, span := p.observe().RunSpan(ctx, r.ID, t.ID, string(t.Kind), r.Attempt, r.Phase)
	defer span.End()
	sctx = context.WithoutCancel(ctx)
	if wait := p.now().Sub(job.CreatedAt); wait > 0 && !job.CreatedAt.IsZero() {
		_, qspan := p.observe().Stage(ctx, obs.StageQueue, obs.AttrInt("job.attempts", job.Attempts))
		qspan.End()
		p.metrics().Stage(ctx, string(t.Kind), obs.StageQueue, wait)
	}
	log = log.With("attempt", r.Attempt, "phase", r.Phase, "mode", r.SessionMode)
	if tid := obs.TraceID(ctx); tid != "" {
		log = log.With("trace_id", tid)
	}

	// Infra failures before evaluation: requeue with backoff, dead-letter after MaxJobAttempts.
	infraFail := func(reason string, backoff time.Duration) error {
		log.Warn("infrastructure failure", "reason", reason)
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusEvaluating, ""); e != nil {
			return e
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusFailed, reason); e != nil {
			return e
		}
		if job.Attempts >= p.Config.MaxJobAttempts {
			msg := fmt.Sprintf("%s (job attempts exhausted: %d)", reason, job.Attempts)
			if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDead, msg); e != nil {
				return e
			}
			p.deadLetter(sctx, t, r.ID, msg)
			p.finished(sctx, t, r.ID)
			return p.Queue.DeadLetter(sctx, job, reason)
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusQueued, reason); e != nil {
			return e
		}
		return p.Queue.Nack(sctx, job, backoff)
	}

	ws, err := p.Workspaces.Provision(ctx, et, r)
	if err != nil {
		return infraFail("provision workspace: "+err.Error(), p.Config.APIErrorBackoff)
	}
	keep := p.Config.KeepWorkspaces
	defer func() {
		if keep {
			log.Info("keeping workspace", "path", ws.Path())
			return
		}
		if derr := ws.Destroy(); derr != nil {
			log.Warn("destroy workspace", "err", derr)
		}
	}()
	configDir := filepath.Join(p.Config.ConfigDirRoot, r.ID)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return infraFail("config dir: "+err.Error(), p.Config.APIErrorBackoff)
	}
	defer func() {
		if !keep {
			_ = os.RemoveAll(configDir) // the transcript was snapshotted by the runner
			p.removeJudgeDirs(configDir)
		}
	}()
	if err := p.Store.RecordWorker(sctx, r.ID, task.WorkerInfo{WorkspacePath: ws.Path(), ConfigDir: configDir, Branch: ws.Branch()}); err != nil {
		return err
	}

	if p.Progress != nil {
		p.Progress.Start(r.ID, t.ID, string(t.Kind))
		defer p.Progress.Forget(r.ID)
	}
	p.metrics().WorkerStarted(ctx, string(t.Kind))
	wctx, wspan := p.observe().Stage(ctx, obs.StageWorker,
		obs.Attr("session.mode", string(r.SessionMode)), obs.Attr("model", et.Policy.Model))
	workerStart := p.now()
	res, runErr := p.Runner.Run(wctx, et, r, runner.Spawn{Workspace: ws.Path(), ConfigDir: configDir})
	p.metrics().WorkerStopped(ctx, string(t.Kind))
	p.metrics().Stage(ctx, string(t.Kind), obs.StageWorker, p.now().Sub(workerStart))
	if res != nil {
		wspan.SetAttributes(obs.Attr("outcome", string(res.Outcome)), obs.AttrInt("events", res.Events),
			obs.AttrInt("tool_uses", res.ToolUses), obs.AttrFloat("cost_usd", res.Metrics().CostUSD))
		if res.Killed != runner.KillNone {
			obs.Fail(wspan, nil, "killed: "+string(res.Killed))
		}
		if res.RateLimited {
			p.metrics().RateLimited(ctx, string(t.Kind))
		}
	}
	obs.Fail(wspan, runErr, "")
	wspan.End()
	if runErr != nil && res == nil {
		return infraFail("spawn worker: "+runErr.Error(), p.Config.APIErrorBackoff)
	}
	if runErr != nil {
		log.Warn("runner returned an error alongside a result", "err", runErr)
	}
	if err := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusEvaluating, ""); err != nil {
		return err
	}
	if err := p.Store.RecordMetrics(sctx, r.ID, res.Metrics()); err != nil {
		return err
	}
	// Spend is recorded from the authoritative result cost, before any routing
	// decision, so a breach stops the next lease even if this run then fails.
	if cost := res.Metrics().CostUSD; cost > 0 {
		if e := p.Budget.Record(sctx, string(t.Kind), cost); e != nil {
			log.Error("recording spend failed", "err", e)
		}
	}
	if err := p.Store.RecordArtifacts(sctx, r.ID, res.EventLogURI, res.SessionURI, nil); err != nil {
		return err
	}
	r.SessionURI = res.SessionURI
	// The resume pointer is the id the transcript was snapshotted under, which
	// is always the id the harness minted and passed to the CLI. The real CLI
	// echoes it back (cli-contract #2); trusting the echo instead would lose
	// the pointer if it ever differed, because no snapshot exists under it.
	if res.SessionURI != "" {
		if err := p.Store.UpdateTaskSession(sctx, t.ID, r.SessionID); err != nil {
			return err
		}
		t.SessionID = r.SessionID
		if res.Result != nil && res.Result.SessionID != "" && res.Result.SessionID != r.SessionID {
			log.Warn("result session_id differs from the minted id; keeping the minted id as the resume pointer",
				"result_session_id", res.Result.SessionID)
		}
	}
	if res.Result != nil && hasJSON(res.Result.StructuredOutput) {
		if err := p.Store.RecordOutput(sctx, r.ID, res.Result.StructuredOutput); err != nil {
			return err
		}
		r.Output = res.Result.StructuredOutput
	}

	// Auth/API failure before the first turn is not a task failure (design §9):
	// back off and requeue without consuming a task attempt.
	if res.Outcome == events.OutcomeAPIError && (res.Result == nil || res.Result.TotalCostUSD == 0) {
		reason := "api_error before first turn"
		switch {
		case res.AuthError != "":
			reason += ": " + res.AuthError + " (check ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN)"
		case res.Result != nil:
			reason += ": " + res.Result.Text()
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusFailed, reason); e != nil {
			return e
		}
		if job.Attempts >= p.Config.MaxJobAttempts {
			msg := reason + " (job attempts exhausted)"
			if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDead, msg); e != nil {
				return e
			}
			p.deadLetter(sctx, t, r.ID, msg)
			p.finished(sctx, t, r.ID)
			return p.Queue.DeadLetter(sctx, job, reason)
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusQueued, reason); e != nil {
			return e
		}
		backoff := p.backoff(job, res)
		log.Warn("api error before first turn; requeued", "backoff", backoff, "rate_limited", res.RateLimited)
		return p.Queue.Nack(sctx, job, backoff)
	}

	// Layer 1.
	in := &checks.Input{Task: et, Run: r, Result: res, Workspace: ws, CommandTimeout: p.Config.CommandTimeout}
	cctx, cspan := p.observe().Stage(ctx, obs.StageChecks)
	checksStart := p.now()
	report, err := checks.Run(cctx, in, p.Checks...)
	p.metrics().Stage(ctx, string(t.Kind), obs.StageChecks, p.now().Sub(checksStart))
	cspan.SetAttributes(obs.AttrBool("failed", report.Failed()), obs.Attr("summary", report.Summary()))
	obs.Fail(cspan, err, "")
	cspan.End()
	if err != nil {
		return infraFail("checks: "+err.Error(), p.Config.APIErrorBackoff)
	}
	ev := task.Eval{Checks: report.Outcomes()}
	if err := p.Store.RecordEval(sctx, r.ID, ev); err != nil {
		return err
	}
	log.Info("checks done", "failed", report.Failed(), "summary", report.Summary())

	// Layer 2: only when Layer 1 passed (§5 short-circuit) and the policy asks for it.
	var verdict *judge.Verdict
	if !report.Failed() && et.Policy.Judge.Enabled {
		jctx, jspan := p.observe().Stage(ctx, obs.StageJudge, obs.AttrInt("samples", et.Policy.Judge.Samples))
		judgeStart := p.now()
		verdict = p.judge(jctx, &judge.Input{Task: et, Run: r, Checks: report, Capture: in.Capture, Outputs: in.Outputs, Result: res,
			Workspace: ws.Path(), ConfigDir: configDir})
		p.metrics().Stage(ctx, string(t.Kind), obs.StageJudge, p.now().Sub(judgeStart))
		jspan.SetAttributes(obs.Attr("verdict", verdict.Verdict), obs.AttrBool("gamed_checks", verdict.GamedChecks),
			obs.AttrFloat("cost_usd", verdict.CostUSD))
		if verdict.Error != "" {
			obs.Fail(jspan, nil, verdict.Error)
		}
		jspan.End()
		p.metrics().JudgeVerdict(ctx, string(t.Kind), verdict.Verdict, verdict.GamedChecks, verdict.CostUSD)
		if verdict.CostUSD > 0 {
			if e := p.Budget.Record(sctx, "judge", verdict.CostUSD); e != nil {
				log.Error("recording judge spend failed", "err", e)
			}
		}
		ev.Judge = verdict.ToTask()
		if err := p.Store.RecordEval(sctx, r.ID, ev); err != nil {
			return err
		}
	}
	r.Eval = &ev

	// Layer 3.
	d := route.Decide(route.Input{Task: et, Run: r, Checks: report, Judge: verdict, DefaultThreshold: p.Config.DefaultThreshold})
	_, rspan := p.observe().Stage(ctx, obs.StageRoute, obs.Attr("decision", string(d.Kind)), obs.Attr("reason", d.Reason))
	rspan.End()
	span.SetAttributes(obs.Attr("route.decision", string(d.Kind)))
	log.Info("routed", "decision", d.Kind, "reason", d.Reason)
	switch d.Kind {
	case route.Retry:
		reason := d.Reason
		if res.SessionLost {
			reason = "session transcript lost; " + reason
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusFailed, reason); e != nil {
			return e
		}
		next, e := p.retry(sctx, t, r, resumable(res), d.Feedback)
		if e != nil {
			return e
		}
		p.metrics().Retry(ctx, string(t.Kind), string(next.SessionMode))
		log.Info("retry scheduled", "next_run_id", next.ID, "next_attempt", next.Attempt, "session_mode", next.SessionMode)
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	case route.DeadLetter:
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusFailed, d.Reason); e != nil {
			return e
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDead, d.Reason); e != nil {
			return e
		}
		p.deadLetter(sctx, t, r.ID, d.Reason)
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	case route.HumanReview:
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusNeedsReview, d.Reason); e != nil {
			return e
		}
		// The workspace holds the change until a human decides (approve delivers from it).
		keep = true
		if e := p.postReview(sctx, t, r, in.Capture, d.Reason); e != nil {
			log.Error("posting review request failed; run waits in needs_review", "err", e)
		}
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	}

	// Fan-out: the planner phase's deliverable is N child tasks, not a branch
	// (design §6, plan Step 21). It runs while the run is still `evaluating`,
	// so a plan that cannot be decomposed can still go to human review —
	// `passed` has no edge to `needs_review`.
	if t.FansOut() && r.Phase == t.FanOutPhase() {
		kids, ferr := p.fanOut(sctx, t, r)
		if ferr != nil {
			if e := p.fanOutFailed(sctx, t, r, ferr); e != nil {
				return e
			}
			p.finished(sctx, t, r.ID)
			return p.Queue.Ack(sctx, job)
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusPassed, d.Reason); e != nil {
			return e
		}
		msg := fmt.Sprintf("fanned out to %d child task(s)", len(kids))
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDelivered, msg); e != nil {
			return e
		}
		log.Info("fanned out", "children", len(kids))
		p.finished(sctx, t, r.ID)
		// A child that failed before this point (bad spec, instant crash) is
		// already terminal, so check the fan-in now rather than waiting a sweep.
		if _, e := p.maybeFanIn(sctx, t.ID); e != nil {
			log.Error("fan-in check after fan-out failed; the sweeper will retry", "err", e)
		}
		return p.Queue.Ack(sctx, job)
	}

	// Deliver.
	if err := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusPassed, d.Reason); err != nil {
		return err
	}
	if t.HasPhases() && r.Phase < len(t.Phases) {
		next, e := p.advancePhase(sctx, t, r, "")
		if e != nil {
			return e
		}
		if e := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDelivered, fmt.Sprintf("phase %d passed; phase %d queued as %s", r.Phase, next.Phase, next.ID)); e != nil {
			return e
		}
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	}
	if p.Deliver == nil {
		log.Warn("no delivery adapter configured; run stays passed")
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	}
	dctx, dspan := p.observe().Stage(ctx, obs.StageDeliver, obs.Attr("adapter", p.Deliver.Name()))
	deliverStart := p.now()
	artifacts, derr := p.Deliver.Deliver(dctx, &deliver.Input{Task: et, Run: r, Workspace: ws, Checks: report, Result: res})
	p.metrics().Stage(ctx, string(t.Kind), obs.StageDeliver, p.now().Sub(deliverStart))
	p.metrics().Delivered(ctx, string(t.Kind), p.Deliver.Name(), derr == nil)
	obs.Fail(dspan, derr, "")
	dspan.SetAttributes(obs.AttrInt("artifacts", len(artifacts)))
	dspan.End()
	if derr != nil {
		// Keep the workspace: the commit exists only there until it is pushed.
		keep = true
		log.Error("delivery failed; workspace kept for operator", "err", derr, "path", ws.Path())
		if e := p.Store.RecordArtifacts(sctx, r.ID, "", "", []string{"workspace:" + ws.Path()}); e != nil {
			return e
		}
		p.finished(sctx, t, r.ID)
		return p.Queue.Ack(sctx, job)
	}
	if err := p.Store.RecordArtifacts(sctx, r.ID, "", "", artifacts); err != nil {
		return err
	}
	if err := p.Store.UpdateRunStatus(sctx, r.ID, task.StatusDelivered, ""); err != nil {
		return err
	}
	log.Info("delivered", "artifacts", artifacts)
	p.finished(sctx, t, r.ID)
	return p.Queue.Ack(sctx, job)
}

// backoff is how long a requeued job waits. A rate-limited result backs off
// exponentially in the job's attempt count and also sheds global concurrency
// for one window, so the pool stops hammering a throttled account (design §9
// "exponential backoff; global concurrency shed").
func (p *Pool) backoff(job *queue.Job, res *runner.Result) time.Duration {
	base := p.Config.APIErrorBackoff
	if base <= 0 {
		base = time.Minute
	}
	if res == nil || !res.RateLimited {
		return base
	}
	d := base
	for i := 1; i < job.Attempts && d < p.maxBackoff(); i++ {
		d *= 2
	}
	if max := p.maxBackoff(); d > max {
		d = max
	}
	p.shed(d)
	return d
}

func (p *Pool) maxBackoff() time.Duration {
	if p.Config.MaxBackoff > 0 {
		return p.Config.MaxBackoff
	}
	return 15 * time.Minute
}

// shed pauses leasing for d (rate limiting). Workers already running finish.
func (p *Pool) shed(d time.Duration) {
	until := p.now().Add(d)
	p.shedMu.Lock()
	if until.After(p.shedUntil) {
		p.shedUntil = until
	}
	p.shedMu.Unlock()
	p.log().Warn("rate limited; shedding concurrency", "until", until, "for", d)
}

// shedding reports whether leasing is paused, and until when.
func (p *Pool) shedding() (bool, time.Time) {
	p.shedMu.Lock()
	defer p.shedMu.Unlock()
	return p.now().Before(p.shedUntil), p.shedUntil
}

// judge runs Layer 2; any failure to obtain a verdict is `uncertain`.
func (p *Pool) judge(ctx context.Context, in *judge.Input) *judge.Verdict {
	if p.Judge == nil {
		return judge.Uncertain("no judge configured")
	}
	v, err := p.Judge.Evaluate(ctx, in)
	if err != nil {
		p.log().Error("judge failed", "run_id", in.Run.ID, "err", err)
		return judge.Uncertain(err.Error())
	}
	return v
}

// resumable reports whether the next attempt can `--resume` this run's
// session: a snapshot exists and the CLI did not report the transcript lost.
func resumable(res *runner.Result) bool {
	return res != nil && res.SessionURI != "" && !res.SessionLost && !res.SessionIDInUse
}

func (p *Pool) removeJudgeDirs(configDir string) {
	matches, _ := filepath.Glob(configDir + "-judge-*")
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
}

func hasJSON(raw []byte) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// deadLetter records an exhausted run for operators and pages (plan Step 18).
// It reads the run back so the entry carries the stored eval evidence.
func (p *Pool) deadLetter(ctx context.Context, t *task.Task, runID, reason string) {
	if p.Dead == nil {
		return
	}
	r, err := p.Store.GetRun(ctx, runID)
	if err != nil {
		p.log().Error("loading the dead run failed", "run_id", runID, "err", err)
		return
	}
	p.metrics().DeadLetter(ctx, string(t.Kind))
	p.Dead.Dead(ctx, deadletter.EntryFor(t, r, reason))
}

// finished records the run's terminal metrics and notifies the hook.
func (p *Pool) finished(ctx context.Context, t *task.Task, runID string) {
	r, err := p.Store.GetRun(ctx, runID)
	if err != nil {
		p.log().Warn("loading the finished run failed", "run_id", runID, "err", err)
		return
	}
	d := time.Duration(r.Metrics.DurationMS) * time.Millisecond
	if r.StartedAt != nil && r.FinishedAt != nil {
		d = r.FinishedAt.Sub(*r.StartedAt)
	}
	p.metrics().RunFinished(ctx, string(t.Kind), string(r.Status), r.Metrics.TerminalReason, d, r.Metrics.Turns, r.Metrics.CostUSD)
	if p.OnRunFinished != nil {
		p.OnRunFinished(t, r)
	}
	// A terminal child may be the last one its parent was waiting for.
	if r.Status.Terminal() {
		p.notifyParent(ctx, t)
	}
}

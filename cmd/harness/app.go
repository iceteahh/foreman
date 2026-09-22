package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/budget"
	"github.com/100xteam-ai/foreman/internal/config"
	"github.com/100xteam-ai/foreman/internal/deadletter"
	"github.com/100xteam-ai/foreman/internal/deliver"
	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/eval/judge"
	"github.com/100xteam-ai/foreman/internal/intake"
	"github.com/100xteam-ai/foreman/internal/obs"
	"github.com/100xteam-ai/foreman/internal/orchestrator"
	"github.com/100xteam-ai/foreman/internal/queue"
	"github.com/100xteam-ai/foreman/internal/review"
	reviewslack "github.com/100xteam-ai/foreman/internal/review/slack"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// backendStore is everything the harness needs from a persistence backend.
// SQLite and Postgres both satisfy it, which is what lets `database.driver`
// move the whole deployment from one node to a fleet without a line changing
// above this seam (plan Step 22).
type backendStore interface {
	store.Store
	review.Store
	budget.Store
	deadletter.Store
	store.Leases
}

// app wires every component from one Config.
type app struct {
	Config config.Config
	Logger *slog.Logger
	Store  backendStore
	Queue  queue.Queue
	Pool   *orchestrator.Pool
	Submit *orchestrator.Submitter
	HTTP   *intake.Server
	Cron   *intake.Cron
	// Replica identifies this process in the singleton leases.
	Replica string
	// Background jobs started by Serve.
	Background []*orchestrator.Periodic
	// Obs owns the metric and trace exporters; always non-nil.
	Obs *obs.Provider
	// Budget is the daily-spend ledger (nil when no ceilings are configured).
	Budget *budget.Ledger
}

func buildApp(cfgPath string, logger *slog.Logger) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cfg.DataRoot, cfg.AuditRoot(), cfg.WorkspaceRoot(), cfg.ConfigDirRoot(), cfg.SessionRoot(), cfg.ReportRoot()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	st, q, err := openBackend(context.Background(), cfg, logger)
	if err != nil {
		return nil, err
	}
	sessions, err := openSessions(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	var aud audit.Sink
	switch cfg.Audit.Sink {
	case "s3":
		s3, err := audit.NewS3Sink(cfg.AuditS3Config())
		if err != nil {
			return nil, err
		}
		s3.Logger = logger
		aud = s3
		logger.Info("audit sink s3", "endpoint", cfg.Audit.Endpoint, "bucket", cfg.Audit.Bucket, "spool", s3.Config.Spool)
	default:
		fs, err := audit.NewFileSink(cfg.AuditRoot())
		if err != nil {
			return nil, err
		}
		aud = fs
	}
	obsProvider, err := obs.New(context.Background(), cfg.ObsConfig(version), logger)
	if err != nil {
		return nil, err
	}

	apiKey := os.Getenv(cfg.Worker.APIKeyEnv)
	oauth := os.Getenv(cfg.Worker.OAuthTokenEnv)
	if apiKey == "" && oauth == "" {
		logger.Warn("no worker credential; isolated workers will fail with api_error", "api_key_env", cfg.Worker.APIKeyEnv, "oauth_token_env", cfg.Worker.OAuthTokenEnv)
	}
	rn := &runner.Runner{
		Bin: cfg.Worker.ClaudeBin, APIKey: apiKey, OAuthToken: oauth, ExtraEnv: cfg.WorkerEnv(),
		Args:     runner.ArgsOptions{BudgetFlagRatio: cfg.Worker.BudgetFlagRatio, PromptViaStdin: cfg.Worker.PromptViaStdin, Bare: cfg.Worker.Bare},
		Grace:    cfg.Grace(),
		Sessions: sessions, Audit: aud, Logger: logger,
	}
	local, err := workspace.NewLocal(cfg.WorkspaceRoot())
	if err != nil {
		return nil, err
	}
	local.RepoBase = cfg.GitHub.RepoBase
	ghToken := os.Getenv(cfg.GitHub.TokenEnv)
	if ghToken != "" {
		local.Tokens = workspace.StaticToken(ghToken)
	}
	for k := range cfg.Env {
		local.Env = append(local.Env, k)
	}
	var wsm workspace.Manager = local

	// Docker mode: one container per run for the worker and for every
	// acceptance command (plan Step 14). Git stays on the host so the repo
	// token is never visible inside a container.
	if cfg.Worker.Mode == "docker" {
		opts := cfg.ContainerOptions()
		opts.Labels = map[string]string{"harness.role": "worker"}
		rn.Launcher = &runner.DockerLauncher{Options: opts, KillTimeout: cfg.DockerKillTimeout()}
		dws, err := workspace.NewDocker(local, opts)
		if err != nil {
			return nil, err
		}
		dws.Logger = logger
		dws.KillTimeout = cfg.DockerKillTimeout()
		for k := range cfg.Env {
			dws.Env = append(dws.Env, k)
		}
		wsm = dws
		logger.Info("worker mode docker", "image", cfg.Worker.Image, "network", cfg.Worker.Docker.Network,
			"egress_proxy", cfg.Worker.Docker.EgressProxy, "host_data_root", cfg.HostDataRoot())
		// Worker containers are siblings: the daemon resolves their bind-mount
		// sources on the host. If the harness is itself containerised without
		// worker.docker.host_data_root, every worker would mount an empty
		// directory and fail its checks with an empty diff, which is a
		// confusing way to find out.
		if cfg.HostDataRoot() == "" && inContainer() {
			logger.Warn("the harness looks containerised but worker.docker.host_data_root is unset; sibling worker containers will mount the wrong directory",
				"data_root", cfg.DataRoot)
		}
	}

	// Kubernetes mode: one Job per run on the worker node pool (plan Step 22).
	// Git still runs here, in the orchestrator, so the repo token never enters
	// a worker pod; the checkout reaches the Job through the shared data
	// volume, and the transcript through object storage.
	if cfg.Worker.Mode == "k8s" {
		opts := cfg.K8sOptions()
		rn.Launcher = &runner.K8sLauncher{Options: opts, StartTimeout: cfg.K8sStartTimeout()}
		logger.Info("worker mode k8s", "image", cfg.Worker.Image, "namespace", opts.Namespace,
			"data_claim", opts.DataClaim, "credential_secret", opts.CredentialSecret, "node_selector", opts.NodeSelector)
		// Acceptance commands still run wherever the orchestrator runs: they
		// execute in the checkout the orchestrator owns, and moving them into
		// a second Job would double the scheduling latency of every run.
	}

	// Version pinning applies to whichever launcher will run (host CLI or image).
	if cfg.CheckVersion() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		v, err := rn.Version(ctx)
		if err != nil {
			return nil, fmt.Errorf("claude version check: %w (set worker.check_version: false to override)", err)
		}
		if v != runner.PinnedCLIVersion {
			return nil, fmt.Errorf("claude CLI version drift: %s reports %s, pinned %s (set worker.check_version: false to override)", cfg.Worker.Mode, v, runner.PinnedCLIVersion)
		}
	}

	var changeAdapter deliver.Adapter = deliver.BranchPush{}
	if ghToken != "" {
		gh, err := deliver.NewGitHubPR(ghToken, cfg.GitHub.APIBase, cfg.DraftPRs())
		if err != nil {
			return nil, err
		}
		gh.Logger = logger
		changeAdapter = &fallbackAdapter{primary: gh, fallback: deliver.BranchPush{}, logger: logger}
	} else {
		logger.Warn("no GitHub token; delivery pushes the bot branch without opening a PR", "env", cfg.GitHub.TokenEnv)
	}

	// Layer 2: the judge shares the runner (same isolation, ephemeral sessions).
	jd := &judge.Judge{Spawner: rn, Logger: logger, Options: judge.Options{
		Model: cfg.Judge.Model, MaxTurns: cfg.Judge.MaxTurns, MaxCostUSD: cfg.Judge.MaxCostUSD, Timeout: cfg.JudgeTimeout(), Parallel: cfg.Judge.Parallel,
	}}

	// Review channel: Slack when credentials are present, otherwise the log.
	var channel review.Channel = review.LogChannel{Logger: logger}
	var slackChannel *reviewslack.Channel
	slackToken, slackSecret := os.Getenv(cfg.Review.SlackBotTokenEnv), os.Getenv(cfg.Review.SlackSigningSecretEnv)
	if slackToken != "" {
		slackChannel = &reviewslack.Channel{Token: slackToken, ChannelID: cfg.Review.SlackChannel, Mention: cfg.Review.EscalationMention, Logger: logger, SLA: cfg.ReviewSLA()}
		channel = slackChannel
		if slackSecret == "" {
			logger.Warn("Slack bot token set but no signing secret; /slack/actions is disabled", "env", cfg.Review.SlackSigningSecretEnv)
		}
	} else {
		logger.Info("no Slack bot token; review requests are logged and decided via `harness review` or POST /runs/{id}/review", "env", cfg.Review.SlackBotTokenEnv)
	}

	// Alerting: Slack when a bot token is present, the log otherwise. One sink
	// serves budget breaches and dead letters (design §8).
	var pager deadletter.Pager = deadletter.LogPager{Logger: logger}
	if slackChannel != nil {
		pager = &reviewslack.Pager{Channel: slackChannel, Mention: cfg.Review.EscalationMention, Logger: logger}
	}

	// Daily spend ceilings (design §8). No ceilings configured → no ledger.
	var ledger *budget.Ledger
	if limits := cfg.BudgetLimits(); len(limits) > 0 {
		ledger = budget.New(st, limits, logger)
		ledger.Pager = pager
		logger.Info("daily budgets", "limits_usd", limits)
	}

	dead := &deadletter.Recorder{Store: st, Pager: pager, Logger: logger}

	// Delivery is chosen by kind (plan Step 20): a change kind produces a
	// branch or pull request, a read-only kind produces a report. Routing them
	// through one adapter would fail every code_review run with "nothing to
	// deliver" even though it did exactly what was asked.
	reportAdapter := deliver.Report{Dir: cfg.ReportRoot()}
	if slackChannel != nil {
		reportAdapter.Notify = &reviewslack.ReportNotifier{Channel: slackChannel, Logger: logger}
	}
	adapter := &deliver.ByKind{Default: changeAdapter, Adapters: map[task.Kind]deliver.Adapter{
		task.KindCodeReview: reportAdapter,
		task.KindReport:     reportAdapter,
		task.KindTriage:     reportAdapter,
		// A fan-out parent never changes a file: its children deliver the
		// branches and its synthesizer delivers the merge report.
		task.KindCodeFixFanout: reportAdapter,
	}}

	// Live progress: the Slack thread per run, or the log.
	var progress *obs.Progress
	switch cfg.Observability.Progress {
	case "off":
	case "slack":
		if slackChannel == nil {
			logger.Warn("observability.progress is slack but no bot token is set; logging progress instead", "env", cfg.Review.SlackBotTokenEnv)
			progress = &obs.Progress{Metrics: obsProvider.Metrics, Interval: cfg.ProgressInterval(), Logger: logger}
		} else {
			progress = &obs.Progress{Sink: &reviewslack.ProgressChannel{Channel: slackChannel}, Metrics: obsProvider.Metrics,
				Interval: cfg.ProgressInterval(), Logger: logger}
		}
	default:
		progress = &obs.Progress{Metrics: obsProvider.Metrics, Interval: cfg.ProgressInterval(), Logger: logger}
	}
	if progress != nil {
		rn.Progress = progress
	}

	submit := &orchestrator.Submitter{Store: st, Queue: q, Sessions: sessions}
	pool := &orchestrator.Pool{
		Store: st, Queue: q, Runner: rn, Workspaces: wsm, Checks: checks.Default(), Judge: jd, Deliver: adapter, Submit: submit,
		Review: channel, Reviews: st, Logger: logger,
		Budget: ledger, Dead: dead, DeadStore: st, Obs: obsProvider, Progress: progress,
		Config: orchestrator.PoolConfig{
			Global: cfg.Concurrency.Global, PerKind: cfg.Concurrency.PerKind,
			PollInterval: cfg.PollInterval(), Lease: 2 * time.Minute,
			MaxJobAttempts: cfg.Queue.MaxJobAttempts, APIErrorBackoff: time.Duration(cfg.Queue.APIErrorBackoffSeconds) * time.Second,
			ConfigDirRoot: cfg.ConfigDirRoot(), KeepWorkspaces: cfg.Retention.Workspaces == "keep",
			CommandTimeout: 10 * time.Minute, DefaultThreshold: cfg.Judge.DefaultThreshold,
			MaxChildren: cfg.FanOut.MaxChildren,
		},
	}
	// The queue and budget gauges are observable: OTel reads them on collection.
	obsProvider.Metrics.ObserveQueue(func() obs.QueueSample {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d, err := q.Depth(ctx)
		if err != nil {
			return obs.QueueSample{}
		}
		return obs.QueueSample{Ready: d.Ready, Leased: d.Leased, Dead: d.Dead, OldestReadyAge: d.OldestReadyAge}
	})
	if ledger != nil {
		obsProvider.Metrics.ObserveSpend(func() []obs.SpendSample {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rows, err := ledger.Report(ctx)
			if err != nil {
				return nil
			}
			out := make([]obs.SpendSample, 0, len(rows))
			for _, r := range rows {
				out = append(out, obs.SpendSample{Key: r.Key, USD: r.USD, Limit: r.Limit})
			}
			return out
		})
	}

	httpSrv := &intake.Server{
		Store: st, Queue: q, Audit: aud, Submit: submit, Logger: logger, Decider: pool, Reviews: st, Budget: ledger, Token: cfg.APIToken(),
		GitHub: intake.GitHubOptions{Secret: os.Getenv(cfg.GitHub.WebhookSecretEnv), TriggerLabel: cfg.GitHub.TriggerLabel, DefaultRef: cfg.GitHub.DefaultRef, Priority: 5, Kind: task.Kind(cfg.GitHub.Kind)},
	}
	if slackChannel != nil && slackSecret != "" {
		httpSrv.Extra = map[string]http.Handler{"POST /slack/actions": &reviewslack.Handler{SigningSecret: slackSecret, Decider: pool, Channel: slackChannel, Logger: logger}}
	}
	// Periodic jobs run on exactly one replica at a time. With SQLite there is
	// only ever one process, but taking the lease anyway keeps one code path.
	replica := orchestrator.ReplicaID()
	escalator := &review.Escalator{Store: st, Reviews: st, Channel: channel, SLA: cfg.ReviewSLA(),
		Interval: time.Duration(cfg.Review.EscalationCheckMinutes) * time.Minute, Logger: logger}
	sweeper := &orchestrator.SessionSweeper{Store: st, Sessions: sessions, Retention: cfg.SessionRetention(), Logger: logger}
	fanIn := &orchestrator.FanInSweeper{Pool: pool, Interval: cfg.FanInSweep(), Logger: logger}
	escalateEvery := time.Duration(cfg.Review.EscalationCheckMinutes) * time.Minute
	if escalateEvery <= 0 {
		escalateEvery = 5 * time.Minute
	}
	var background []*orchestrator.Periodic
	// Escalation needs an SLA to measure against; without one the job would
	// wake every few minutes to do nothing.
	if cfg.ReviewSLA() > 0 {
		background = append(background, &orchestrator.Periodic{
			Name: orchestrator.LeaseEscalator, What: "review SLA escalation", Interval: escalateEvery,
			Once: escalator.Once, Leases: st, Owner: replica, Logger: logger})
	}
	background = append(background, &orchestrator.Periodic{
		Name: orchestrator.LeaseFanIn, What: "fan-in sweep", Interval: cfg.FanInSweep(),
		Once: fanIn.Once, Leases: st, Owner: replica, Logger: logger})
	if cfg.SessionRetention() > 0 {
		background = append(background, &orchestrator.Periodic{
			Name: orchestrator.LeaseSessions, What: "session snapshot sweep", Interval: time.Hour,
			Once: sweeper.Once, Leases: st, Owner: replica, Logger: logger})
	}
	if httpSrv.GitHub.Secret == "" {
		logger.Warn("GitHub webhook secret is empty; signatures are not verified", "env", cfg.GitHub.WebhookSecretEnv)
	}
	cr, err := intake.NewCron(cfg.Cron, submit, logger)
	if err != nil {
		return nil, err
	}
	// Every replica holds the same schedule, so a firing is gated on a lease:
	// otherwise a nightly report becomes N nightly reports and N budgets.
	cr.Gate = orchestrator.LeaseGate(st, replica, 2*time.Minute, logger)
	return &app{Config: cfg, Logger: logger, Store: st, Queue: q, Pool: pool, Submit: submit, HTTP: httpSrv, Cron: cr,
		Replica: replica, Background: background, Obs: obsProvider, Budget: ledger}, nil
}

// Close flushes the exporters and releases the store.
func (a *app) Close() {
	if a.Obs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Obs.Shutdown(ctx); err != nil {
			a.Logger.Warn("flushing telemetry failed", "err", err)
		}
	}
	_ = a.Store.Close()
}

// Serve runs HTTP + cron + pool until ctx is cancelled, then drains.
func (a *app) Serve(ctx context.Context) error {
	// An open API on a routable address lets anyone submit write-capable
	// tasks and approve runs, so this is a startup failure, not a warning.
	if err := a.Config.APIAuthError(); err != nil {
		return err
	}
	if a.HTTP.Token == "" {
		a.Logger.Warn("API bearer token unset; the API is open to local callers", "addr", a.Config.Server.Addr, "env", a.Config.Server.APITokenEnv)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.Config.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.Config.Server.Addr, err)
	}
	srv := &http.Server{Handler: a.HTTP.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		a.Logger.Info("http listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	a.Cron.Start()
	a.Logger.Info("harness started", "workers", a.Config.Concurrency.Global, "cron_entries", a.Cron.Len(),
		"data_root", a.Config.DataRoot, "replica", a.Replica, "store", a.Config.Database.Driver, "worker_mode", a.Config.Worker.Mode)

	poolDone := make(chan struct{})
	go func() {
		_ = a.Pool.Run(ctx)
		close(poolDone)
	}()
	for _, p := range a.Background {
		go p.Run(ctx)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	a.Logger.Info("shutting down: draining in-flight runs")
	<-a.Cron.Stop().Done()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	<-poolDone
	a.Logger.Info("harness stopped")
	return nil
}

// RunOnce submits spec and processes jobs until the task rests: its newest
// run is delivered, dead, closed, waiting for review, or failed with no retry.
func (a *app) RunOnce(ctx context.Context, specJSON []byte) (*task.Run, error) {
	var spec task.Spec
	dec := json.NewDecoder(bytesReader(specJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("task spec: %w", err)
	}
	if spec.RequestedBy == "" {
		spec.RequestedBy = "cli:run-once"
	}
	t, r, err := a.Submit.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	a.Logger.Info("submitted", "task_id", t.ID, "run_id", r.ID)
	return a.DriveTask(ctx, t.ID)
}

// DriveTask processes jobs until the task's newest run rests (see RunOnce).
// Retries and phase advances create new runs synchronously, so a newest run
// in `failed` has no successor and the task is done for now.
func (a *app) DriveTask(ctx context.Context, taskID string) (*task.Run, error) {
	for {
		if _, err := a.Pool.ProcessOne(ctx, ctx); err != nil {
			return a.latestRun(ctx, taskID, err)
		}
		cur, err := a.Store.LatestRun(ctx, taskID)
		if err != nil {
			return nil, err
		}
		switch cur.Status {
		case task.StatusDelivered, task.StatusDead, task.StatusClosed, task.StatusFailed, task.StatusPassed, task.StatusNeedsReview:
			// A fan-out parent rests at its planner run while the children are
			// still running (plan Step 21). The task is not done until they
			// finish and the synthesizer has had its turn.
			pending, err := a.Pool.FanOutPending(ctx, taskID)
			if err != nil {
				return a.latestRun(ctx, taskID, err)
			}
			if !pending {
				return cur, nil
			}
		}
		select {
		case <-ctx.Done():
			return a.latestRun(ctx, taskID, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (a *app) latestRun(ctx context.Context, taskID string, err error) (*task.Run, error) {
	cur, gerr := a.Store.LatestRun(context.WithoutCancel(ctx), taskID)
	if gerr != nil {
		return nil, errors.Join(err, gerr)
	}
	return cur, err
}

// inContainer reports whether this process is itself running in a container,
// which changes how worker bind mounts must be addressed.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil && strings.Contains(string(b), "docker") {
		return true
	}
	return false
}

// fallbackAdapter uses the PR adapter for GitHub-shaped repos and plain branch
// push for anything else (local bare repos in demos).
type fallbackAdapter struct {
	primary  *deliver.GitHubPR
	fallback deliver.Adapter
	logger   *slog.Logger
}

func (f *fallbackAdapter) Name() string { return "github_pr_or_branch" }

func (f *fallbackAdapter) Deliver(ctx context.Context, in *deliver.Input) ([]string, error) {
	if _, _, err := deliver.OwnerRepo(in.Task.Workspace.Repo); err != nil {
		f.logger.Info("repo is not GitHub-shaped; pushing branch only", "repo", in.Task.Workspace.Repo)
		return f.fallback.Deliver(ctx, in)
	}
	return f.primary.Deliver(ctx, in)
}

// Package config loads harness.yaml (design §11) with defaults.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/container"
	"github.com/100xteam-ai/foreman/internal/egress"
	"github.com/100xteam-ai/foreman/internal/k8s"
	"github.com/100xteam-ai/foreman/internal/obs"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/session"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Config is the whole harness.yaml.
type Config struct {
	Server        Server            `yaml:"server"`
	DataRoot      string            `yaml:"data_root"`
	Database      Database          `yaml:"database"`
	Concurrency   Concurrency       `yaml:"concurrency"`
	Budgets       Budgets           `yaml:"budgets"`
	Worker        Worker            `yaml:"worker"`
	Session       Session           `yaml:"session"`
	Judge         Judge             `yaml:"judge"`
	Review        Review            `yaml:"review"`
	Egress        Egress            `yaml:"egress"`
	Observability Observability     `yaml:"observability"`
	Audit         Audit             `yaml:"audit"`
	Retention     Retention         `yaml:"retention"`
	GitHub        GitHub            `yaml:"github"`
	FanOut        FanOut            `yaml:"fanout"`
	Queue         Queue             `yaml:"queue"`
	Cron          []CronEntry       `yaml:"cron"`
	Env           map[string]string `yaml:"env"` // extra allowlisted vars for workers (toolchain)
}

type Server struct {
	Addr string `yaml:"addr"`
	// APITokenEnv names the variable holding the bearer token every API route
	// requires, except GET /healthz and POST /webhooks/github (which keeps its
	// HMAC). It is a variable name, never the token. The harness refuses to
	// serve without a token unless Addr is loopback: the same listener carries
	// the webhook, so it is routinely exposed, and an open API lets anyone
	// submit a write-capable task or approve any run.
	APITokenEnv string `yaml:"api_token_env"`
}

type Concurrency struct {
	Global  int            `yaml:"global"`
	PerKind map[string]int `yaml:"per_kind"`
}

type Budgets struct {
	PerRunUSDDefault float64            `yaml:"per_run_usd_default"`
	DailyUSD         map[string]float64 `yaml:"daily_usd"`
}

type Worker struct {
	// Mode is "local" (spawn the host CLI) or "docker" (one container per run,
	// plan Step 14). Docker mode requires worker.image.
	Mode      string `yaml:"mode"`
	ClaudeBin string `yaml:"claude_bin"`
	// Image is the worker image in docker mode (harness/worker:cli-<pinned>).
	Image            string  `yaml:"image"`
	DefaultTimeoutMS int64   `yaml:"default_timeout_ms"`
	DefaultMaxTurns  int     `yaml:"default_max_turns"`
	BudgetFlagRatio  float64 `yaml:"budget_flag_ratio"`
	APIKeyEnv        string  `yaml:"api_key_env"`
	// OAuthTokenEnv names the variable holding a `claude setup-token` token
	// (sk-ant-oat01-…). Either credential is enough; the API key wins if both are set.
	OAuthTokenEnv  string `yaml:"oauth_token_env"`
	GraceMS        int64  `yaml:"grace_ms"`
	PromptViaStdin bool   `yaml:"prompt_via_stdin"`
	Bare           bool   `yaml:"bare"`
	// CheckVersion fails startup when the installed CLI is not the pinned one.
	CheckVersion *bool `yaml:"check_version"`
	// Docker configures the container sandbox (worker.mode: docker).
	Docker DockerWorker `yaml:"docker"`
	// K8s configures the Kubernetes Job sandbox (worker.mode: k8s).
	K8s K8sWorker `yaml:"k8s"`
}

// DockerWorker is the container sandbox (design §4.1).
type DockerWorker struct {
	// Bin is the docker client (default "docker").
	Bin string `yaml:"bin"`
	// Network is the docker network workers join. Create it `--internal` so the
	// only route out is the egress proxy; "none" disables networking entirely.
	Network string `yaml:"network"`
	// User is --user (uid[:gid]); "host" resolves to the harness's own ids,
	// which is what keeps bind-mounted files writable on Linux. Empty keeps
	// the image user (correct on Docker Desktop).
	User string `yaml:"user"`
	// EgressProxy is exported as HTTPS_PROXY/HTTP_PROXY inside the container.
	EgressProxy string  `yaml:"egress_proxy"`
	NoProxy     string  `yaml:"no_proxy"`
	Memory      string  `yaml:"memory"`
	CPUs        float64 `yaml:"cpus"`
	PidsLimit   int     `yaml:"pids_limit"`
	// ReadOnlyRoot mounts the image read-only with tmpfs on /tmp and HOME.
	ReadOnlyRoot bool `yaml:"read_only_root"`
	// ExtraArgs are appended to every `docker run` (operator escape hatch).
	ExtraArgs []string `yaml:"extra_args"`
	// KillTimeoutMS bounds the stop loop after a timeout (default 120000).
	KillTimeoutMS int64 `yaml:"kill_timeout_ms"`
	// HostDataRoot is data_root as the **host** sees it. Set it only when the
	// harness itself runs in a container: worker containers are siblings
	// started through the host daemon, which resolves every bind-mount source
	// on the host, so the in-container path would mount the wrong directory.
	// A "$NAME" value is read from the environment.
	HostDataRoot string `yaml:"host_data_root"`
}

// Database selects the persistence backend (plan Step 22, design §10).
// "sqlite" is the single-node install; "postgres" is the scaled topology,
// where several stateless orchestrator replicas share one managed database.
type Database struct {
	// Driver is "sqlite" (default) or "postgres".
	Driver string `yaml:"driver"`
	// DSNEnv names the variable holding the Postgres connection string. It is
	// a variable name, never the DSN itself: the DSN carries a password.
	DSNEnv string `yaml:"dsn_env"`
	// DSN is a literal connection string for local development. A "$NAME"
	// value is read from the environment (the `env:` convention).
	DSN string `yaml:"dsn"`
}

// Session selects the transcript snapshot store (design §4.4).
type Session struct {
	// Store is "local" (a directory) or "s3" (object storage). Workers that
	// run as Kubernetes Jobs need "s3": the next attempt may land on another
	// node, where a local directory no longer exists.
	Store string `yaml:"store"`
	Root  string `yaml:"root"`
	// S3 settings; Endpoint and Bucket are required for store: s3. Credentials
	// come from the audit section's env vars, or the instance/IRSA chain.
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	Prefix   string `yaml:"prefix"`
	Region   string `yaml:"region"`
	UseSSL   *bool  `yaml:"use_ssl"`
	// AccessKeyEnv / SecretKeyEnv name the credential variables; empty falls
	// back to the AWS environment and instance credential chain.
	AccessKeyEnv string `yaml:"access_key_env"`
	SecretKeyEnv string `yaml:"secret_key_env"`
}

// K8sWorker runs each worker as a Kubernetes Job (worker.mode: k8s).
type K8sWorker struct {
	// Kubectl is the client executable (default "kubectl").
	Kubectl    string `yaml:"kubectl"`
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
	Namespace  string `yaml:"namespace"`
	// CredentialSecret names the Secret whose keys become the worker's
	// environment. It is the only way a credential reaches a worker Job:
	// nothing sensitive is ever written into a manifest.
	CredentialSecret string `yaml:"credential_secret"`
	ServiceAccount   string `yaml:"service_account"`
	// DataClaim is the ReadWriteMany PVC holding data_root. The orchestrator
	// writes each checkout into it and the worker Job mounts that subPath, so
	// the claim must support being mounted by both pods at once.
	DataClaim string `yaml:"data_claim"`
	// ImagePullPolicy for the worker container (default IfNotPresent).
	ImagePullPolicy string            `yaml:"image_pull_policy"`
	NodeSelector    map[string]string `yaml:"node_selector"`
	Tolerations     []map[string]any  `yaml:"tolerations"`
	Annotations     map[string]string `yaml:"annotations"`
	CPURequest      string            `yaml:"cpu_request"`
	CPULimit        string            `yaml:"cpu_limit"`
	MemoryRequest   string            `yaml:"memory_request"`
	MemoryLimit     string            `yaml:"memory_limit"`
	RunAsUser       int64             `yaml:"run_as_user"`
	FSGroup         int64             `yaml:"fs_group"`
	ReadOnlyRoot    bool              `yaml:"read_only_root"`
	EgressProxy     string            `yaml:"egress_proxy"`
	NoProxy         string            `yaml:"no_proxy"`
	// TTLSeconds deletes a finished Job (default 600).
	TTLSeconds int64 `yaml:"ttl_seconds"`
	// StartTimeoutSeconds bounds waiting for the pod to be scheduled and
	// pulled (default 300).
	StartTimeoutSeconds int64 `yaml:"start_timeout_seconds"`
}

type Judge struct {
	ModelFlagPassthrough []string `yaml:"model_flag_passthrough"`
	DefaultThreshold     int      `yaml:"default_threshold"`
	// Model is the judge model when policy.judge.model is empty ("" = CLI default).
	Model string `yaml:"model"`
	// Bounds of one judge invocation (design §5.2: small).
	MaxTurns   int     `yaml:"max_turns"`
	MaxCostUSD float64 `yaml:"max_cost_usd"`
	TimeoutMS  int64   `yaml:"timeout_ms"`
	// Parallel runs judge samples concurrently.
	Parallel bool `yaml:"parallel"`
}

type Review struct {
	SlackChannel string `yaml:"slack_channel"`
	SLAHours     int    `yaml:"sla_hours"`
	// SlackBotTokenEnv / SlackSigningSecretEnv name the variables holding the
	// bot token (xoxb-…) and the app signing secret. Both empty → log channel.
	SlackBotTokenEnv      string `yaml:"slack_bot_token_env"`
	SlackSigningSecretEnv string `yaml:"slack_signing_secret_env"`
	// EscalationMention is prepended to SLA escalations ("<!here>", "<@U…>").
	EscalationMention string `yaml:"escalation_mention"`
	// EscalationCheckMinutes is how often unanswered reviews are scanned.
	EscalationCheckMinutes int `yaml:"escalation_check_minutes"`
}

// Observability configures metrics, traces and the live progress feed
// (design §8, plan Step 16).
type Observability struct {
	Enabled bool `yaml:"enabled"`
	// OTLPEndpoint is the collector ("localhost:4318"); empty keeps metrics local.
	OTLPEndpoint string `yaml:"otlp_endpoint"`
	Insecure     bool   `yaml:"otlp_insecure"`
	// PrometheusAddr serves /metrics (":9464"); empty disables it.
	PrometheusAddr   string `yaml:"prometheus_addr"`
	MetricIntervalMS int64  `yaml:"metric_interval_ms"`
	// TraceSampleRatio is head sampling (default 1: every run).
	TraceSampleRatio float64 `yaml:"trace_sample_ratio"`
	Environment      string  `yaml:"environment"`
	// Progress publishes the live run timeline: "off", "log" or "slack".
	Progress string `yaml:"progress"`
	// ProgressIntervalMS throttles progress updates (default 15000).
	ProgressIntervalMS int64 `yaml:"progress_interval_ms"`
}

// Audit selects the audit sink (plan Step 17).
type Audit struct {
	// Sink is "file" (default) or "s3".
	Sink string `yaml:"sink"`
	// S3 settings; Endpoint and Bucket are required for sink: s3.
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	Prefix   string `yaml:"prefix"`
	Region   string `yaml:"region"`
	// AccessKeyEnv / SecretKeyEnv name the variables holding the credentials;
	// empty falls back to the AWS environment and instance credential chain.
	AccessKeyEnv string `yaml:"access_key_env"`
	SecretKeyEnv string `yaml:"secret_key_env"`
	// UseSSL defaults to true; set false for a local MinIO over http.
	UseSSL *bool `yaml:"use_ssl"`
	// Spool is where logs are written before upload (default <data_root>/audit).
	Spool string `yaml:"spool"`
}

// Egress configures the `harness egress` allowlisting proxy (design §4.1).
type Egress struct {
	Addr string `yaml:"addr"`
	// Allow lists host names; "*.suffix" matches subdomains. Empty = the
	// built-in default (Anthropic API, GitHub, Go and npm registries).
	Allow []string `yaml:"allow"`
	// Ports restricts destination ports (default 443 and 80).
	Ports []int `yaml:"ports"`
}

type Retention struct {
	AuditNDJSONDays int    `yaml:"audit_ndjson_days"`
	SessionsDays    int    `yaml:"sessions_days"`
	Workspaces      string `yaml:"workspaces"` // destroy_on_capture | keep
}

type GitHub struct {
	TokenEnv         string `yaml:"token_env"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	APIBase          string `yaml:"api_base"` // GHES; empty = api.github.com
	RepoBase         string `yaml:"repo_base"`
	DraftPRs         *bool  `yaml:"draft_prs"`
	// TriggerLabel: issues labelled with it become code_fix tasks ("" = every opened issue).
	TriggerLabel string `yaml:"trigger_label"`
	// DefaultRef is the branch tasks start from when the webhook payload has none.
	DefaultRef string `yaml:"default_ref"`
	// Kind is the task kind webhook issues become (code_fix or code_fix_planned).
	Kind string `yaml:"kind"`
}

// FanOut bounds the fan-out workflow (plan Step 21, design §6).
type FanOut struct {
	// MaxChildren caps one decomposition regardless of the kind's child
	// template (0 = whatever the template allows). It is the runaway guard:
	// a planner that asks for 40 workers spends 40 budgets.
	MaxChildren int `yaml:"max_children"`
	// SweepMinutes is how often the fan-in sweeper looks for parents whose
	// children all finished while nothing was watching (default 5).
	SweepMinutes int `yaml:"sweep_minutes"`
}

type Queue struct {
	PollIntervalMS int64 `yaml:"poll_interval_ms"`
	// LeaseExtraSeconds is added to the task timeout to size the lease.
	LeaseExtraSeconds int64 `yaml:"lease_extra_seconds"`
	MaxJobAttempts    int   `yaml:"max_job_attempts"`
	// APIErrorBackoffSeconds delays a requeue after an api_error result.
	APIErrorBackoffSeconds int64 `yaml:"api_error_backoff_seconds"`
}

// CronEntry schedules a task spec.
type CronEntry struct {
	Name     string         `yaml:"name"`
	Schedule string         `yaml:"schedule"`
	Task     map[string]any `yaml:"task"` // same shape as POST /tasks
}

// ptr returns a fresh pointer. Every *bool default must get its own allocation:
// yaml.v3 writes through an existing pointer, so two fields sharing one would
// flip together (check_version: false used to disable draft PRs).
func ptr[T any](v T) *T { return &v }

// Default returns the design §11 defaults for a single-node local install.
func Default() Config {
	return Config{
		Server:      Server{Addr: ":8080", APITokenEnv: "HARNESS_API_TOKEN"}, //nolint:gosec // env var name, not a secret
		DataRoot:    ".harness",
		Concurrency: Concurrency{Global: 2, PerKind: map[string]int{"code_fix": 2, "code_review": 2, "report": 1, "triage": 1}},
		Budgets:     Budgets{PerRunUSDDefault: 2.5, DailyUSD: map[string]float64{"code_fix": 60, "code_review": 30, "report": 25, "triage": 15, "global": 120}},
		Worker: Worker{Mode: "local", ClaudeBin: "claude", DefaultTimeoutMS: 900000, DefaultMaxTurns: 30, BudgetFlagRatio: 0.9, //nolint:gosec // env var name below, not a secret
			APIKeyEnv: "ANTHROPIC_API_KEY", OAuthTokenEnv: "CLAUDE_CODE_OAUTH_TOKEN", GraceMS: 10000, CheckVersion: ptr(true),
			Image: "harness/worker:cli-" + runner.PinnedCLIVersion,
			Docker: DockerWorker{Bin: "docker", Network: "harness-internal", User: "host", EgressProxy: "http://egress:3128",
				Memory: "4g", CPUs: 2, PidsLimit: 512, KillTimeoutMS: 120000},
			K8s: K8sWorker{Kubectl: "kubectl", Namespace: "harness", CredentialSecret: "harness-worker", DataClaim: "harness-data",
				ImagePullPolicy: "IfNotPresent", CPURequest: "500m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "4Gi",
				RunAsUser: 1000, FSGroup: 1000, TTLSeconds: 600, StartTimeoutSeconds: 300,
				NodeSelector: map[string]string{"harness.io/pool": "workers"}}},
		Database: Database{Driver: "sqlite", DSNEnv: "HARNESS_POSTGRES_DSN"},
		Session:  Session{Store: "local", AccessKeyEnv: "AWS_ACCESS_KEY_ID", SecretKeyEnv: "AWS_SECRET_ACCESS_KEY"}, //nolint:gosec // env var names, not secrets
		Judge:    Judge{DefaultThreshold: 7, MaxTurns: 12, MaxCostUSD: 0.25, TimeoutMS: 180000},
		Egress:   Egress{Addr: ":3128"},
		Observability: Observability{Progress: "log", MetricIntervalMS: 30000, TraceSampleRatio: 1,
			ProgressIntervalMS: 15000, PrometheusAddr: ":9464", Environment: "local"},
		Audit: Audit{Sink: "file", AccessKeyEnv: "AWS_ACCESS_KEY_ID", SecretKeyEnv: "AWS_SECRET_ACCESS_KEY"}, //nolint:gosec // env var names, not secrets
		Review: Review{SlackChannel: "#agent-review", SLAHours: 24, SlackBotTokenEnv: "SLACK_BOT_TOKEN", SlackSigningSecretEnv: "SLACK_SIGNING_SECRET", //nolint:gosec // env var names, not secrets
			EscalationMention: "<!here>", EscalationCheckMinutes: 5},
		Retention: Retention{AuditNDJSONDays: 365, SessionsDays: 30, Workspaces: "destroy_on_capture"},
		GitHub:    GitHub{TokenEnv: "GITHUB_TOKEN", WebhookSecretEnv: "GITHUB_WEBHOOK_SECRET", RepoBase: "https://github.com/", DraftPRs: ptr(true), TriggerLabel: "harness", DefaultRef: "main", Kind: "code_fix"}, //nolint:gosec // env var names, not secrets
		FanOut:    FanOut{MaxChildren: task.DefaultMaxChildren, SweepMinutes: 5},
		Queue:     Queue{PollIntervalMS: 1000, LeaseExtraSeconds: 300, MaxJobAttempts: 5, APIErrorBackoffSeconds: 60},
	}
}

// Load reads path on top of Default(). A missing file yields the defaults.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, c.Validate()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, c.Validate()
		}
		return c, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if !filepath.IsAbs(c.DataRoot) {
		// The base must be absolute: the worker is spawned with its cwd set to
		// the workspace (runner.launch), so a relative data_root would resolve
		// against the checkout rather than the harness's directory. -config is
		// routinely relative, and filepath.Dir("harness.yaml") is ".".
		base, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return c, fmt.Errorf("resolve config dir %s: %w", path, err)
		}
		c.DataRoot = filepath.Join(base, c.DataRoot)
	}
	if repo, inside := repoHolding(c.DataRoot); inside {
		return c, fmt.Errorf("data_root %s is inside the repository %s: workspaces are checked out under it and the CLI walks parent directories for CLAUDE.md and .claude/, so every worker would inherit that repository's own instructions; move it outside the repo (e.g. ../foreman-data)", c.DataRoot, repo)
	}
	return c, c.Validate()
}

// Validate checks the values the orchestrator relies on.
func (c Config) Validate() error {
	var errs []error
	if c.Concurrency.Global <= 0 {
		errs = append(errs, errors.New("concurrency.global must be > 0"))
	}
	if c.Worker.BudgetFlagRatio <= 0 || c.Worker.BudgetFlagRatio > 1 {
		errs = append(errs, errors.New("worker.budget_flag_ratio must be in (0,1]"))
	}
	switch c.Worker.Mode {
	case "local":
	case "docker":
		if strings.TrimSpace(c.Worker.Image) == "" {
			errs = append(errs, errors.New("worker.mode docker needs worker.image"))
		}
		if err := c.ContainerOptions().Validate(); err != nil {
			errs = append(errs, err)
		}
	case "k8s":
		if strings.TrimSpace(c.Worker.Image) == "" {
			errs = append(errs, errors.New("worker.mode k8s needs worker.image"))
		}
		if err := c.K8sOptions().Validate(); err != nil {
			errs = append(errs, err)
		}
		if c.Worker.PromptViaStdin {
			errs = append(errs, errors.New("worker.prompt_via_stdin is not supported in k8s mode (a Job has no stdin to attach)"))
		}
		// A worker Job may land on any node, so a transcript written to a
		// local directory is gone on the next attempt: every retry would be a
		// cold start, silently losing the session-resume design (§4.4).
		if c.Session.Store != "s3" {
			errs = append(errs, errors.New("worker.mode k8s needs session.store: s3 (a Job's transcript cannot live on one node's disk)"))
		}
	default:
		errs = append(errs, fmt.Errorf("worker.mode %q unsupported (local|docker|k8s)", c.Worker.Mode))
	}
	switch c.Database.Driver {
	case "", "sqlite":
	case "postgres":
		if c.PostgresDSN() == "" {
			errs = append(errs, fmt.Errorf("database.driver postgres needs a DSN in database.dsn or $%s", c.Database.DSNEnv))
		}
	default:
		errs = append(errs, fmt.Errorf("database.driver %q must be sqlite or postgres", c.Database.Driver))
	}
	switch c.Session.Store {
	case "", "local":
	case "s3":
		if strings.TrimSpace(c.Session.Endpoint) == "" || strings.TrimSpace(c.Session.Bucket) == "" {
			errs = append(errs, errors.New("session.store s3 needs session.endpoint and session.bucket"))
		}
	default:
		errs = append(errs, fmt.Errorf("session.store %q must be local or s3", c.Session.Store))
	}
	if c.Queue.PollIntervalMS <= 0 {
		errs = append(errs, errors.New("queue.poll_interval_ms must be > 0"))
	}
	if c.FanOut.MaxChildren < 0 {
		errs = append(errs, errors.New("fanout.max_children must be >= 0"))
	}
	for _, e := range c.Cron {
		if e.Schedule == "" || e.Task == nil {
			errs = append(errs, fmt.Errorf("cron entry %q needs schedule and task", e.Name))
		}
	}
	switch c.Observability.Progress {
	case "", "off", "log", "slack":
	default:
		errs = append(errs, fmt.Errorf("observability.progress %q must be off, log or slack", c.Observability.Progress))
	}
	if r := c.Observability.TraceSampleRatio; r < 0 || r > 1 {
		errs = append(errs, errors.New("observability.trace_sample_ratio must be in [0,1]"))
	}
	switch c.Audit.Sink {
	case "", "file":
	case "s3":
		if strings.TrimSpace(c.Audit.Endpoint) == "" || strings.TrimSpace(c.Audit.Bucket) == "" {
			errs = append(errs, errors.New("audit.sink s3 needs audit.endpoint and audit.bucket"))
		}
	default:
		errs = append(errs, fmt.Errorf("audit.sink %q must be file or s3", c.Audit.Sink))
	}
	if c.Judge.DefaultThreshold < 0 || c.Judge.DefaultThreshold > 10 {
		errs = append(errs, errors.New("judge.default_threshold must be in 0..10"))
	}
	// An issue webhook may open any kind whose prompt template takes the issue
	// payload: fix it, review it, report on it, or just classify it.
	if k := task.Kind(c.GitHub.Kind); c.GitHub.Kind != "" && !k.Valid() {
		errs = append(errs, fmt.Errorf("github.kind %q is not a known task kind %v", c.GitHub.Kind, task.Kinds))
	}
	// Without a label, every opened issue starts a paid run with a prompt the
	// issue's author wrote. The label is what makes a maintainer's decision the
	// trigger rather than the act of filing an issue.
	if strings.TrimSpace(c.GitHub.TriggerLabel) == "" {
		errs = append(errs, errors.New("github.trigger_label is required: without it anyone who can open an issue can start a run"))
	}
	if strings.TrimSpace(c.Server.APITokenEnv) == "" {
		errs = append(errs, errors.New("server.api_token_env must name the variable holding the API bearer token"))
	}
	return errors.Join(errs...)
}

// JudgeTimeout is the per-invocation judge deadline.
func (c Config) JudgeTimeout() time.Duration {
	return time.Duration(c.Judge.TimeoutMS) * time.Millisecond
}

// FanInSweep is how often the fan-in sweeper runs.
func (c Config) FanInSweep() time.Duration {
	if c.FanOut.SweepMinutes <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.FanOut.SweepMinutes) * time.Minute
}

// ReviewSLA is the escalation delay (0 disables).
func (c Config) ReviewSLA() time.Duration { return time.Duration(c.Review.SLAHours) * time.Hour }

// SessionRetention is how long snapshots of terminal tasks are kept (0 disables the sweep).
func (c Config) SessionRetention() time.Duration {
	return time.Duration(c.Retention.SessionsDays) * 24 * time.Hour
}

// Paths derived from DataRoot.
func (c Config) DBPath() string        { return filepath.Join(c.DataRoot, "harness.db") }
func (c Config) AuditRoot() string     { return filepath.Join(c.DataRoot, "audit") }
func (c Config) WorkspaceRoot() string { return filepath.Join(c.DataRoot, "workspaces") }
func (c Config) ConfigDirRoot() string { return filepath.Join(c.DataRoot, "claude-config") }
func (c Config) EvalRoot() string      { return filepath.Join(c.DataRoot, "eval") }
func (c Config) ReportRoot() string    { return filepath.Join(c.DataRoot, "reports") }
func (c Config) SessionRoot() string {
	if c.Session.Root != "" {
		return c.Session.Root
	}
	return filepath.Join(c.DataRoot, "sessions")
}

// PollInterval as a duration.
func (c Config) PollInterval() time.Duration {
	return time.Duration(c.Queue.PollIntervalMS) * time.Millisecond
}

// Grace is the SIGTERM→SIGKILL delay.
func (c Config) Grace() time.Duration { return time.Duration(c.Worker.GraceMS) * time.Millisecond }

// DraftPRs defaults to true.
func (c Config) DraftPRs() bool { return c.GitHub.DraftPRs == nil || *c.GitHub.DraftPRs }

// APIToken is the bearer token the intake API requires, read from
// server.api_token_env. Empty means the API is unauthenticated, which
// APIAuthError only allows on a loopback listener.
func (c Config) APIToken() string { return strings.TrimSpace(os.Getenv(c.Server.APITokenEnv)) }

// APIAuthError is why `serve` must not start: no bearer token while the bind
// address is reachable from other hosts. A loopback listener may run open,
// which is what the demos and a developer's laptop do.
func (c Config) APIAuthError() error {
	if c.APIToken() != "" || isLoopbackAddr(c.Server.Addr) {
		return nil
	}
	return fmt.Errorf("server.addr %s is not loopback and $%s is unset: the API would accept tasks and review decisions from anyone who can reach the port; set the token (openssl rand -hex 32) or bind 127.0.0.1", c.Server.Addr, c.Server.APITokenEnv)
}

// isLoopbackAddr reports whether a listen address only accepts local
// connections. An empty host (":8080") binds every interface.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckVersion defaults to true.
func (c Config) CheckVersion() bool { return c.Worker.CheckVersion == nil || *c.Worker.CheckVersion }

// ObsConfig renders the observability section for internal/obs.
func (c Config) ObsConfig(version string) obs.Config {
	return obs.Config{
		Enabled: c.Observability.Enabled, OTLPEndpoint: c.Observability.OTLPEndpoint, Insecure: c.Observability.Insecure,
		PrometheusAddr:   c.Observability.PrometheusAddr,
		MetricInterval:   time.Duration(c.Observability.MetricIntervalMS) * time.Millisecond,
		TraceSampleRatio: c.Observability.TraceSampleRatio, Version: version, Environment: c.Observability.Environment,
	}
}

// ProgressInterval throttles live progress updates.
func (c Config) ProgressInterval() time.Duration {
	return time.Duration(c.Observability.ProgressIntervalMS) * time.Millisecond
}

// AuditS3Config renders the audit section for audit.NewS3Sink.
func (c Config) AuditS3Config() audit.S3Config {
	a := c.Audit
	spool := a.Spool
	if spool == "" {
		spool = c.AuditRoot()
	}
	return audit.S3Config{
		Endpoint: a.Endpoint, Bucket: a.Bucket, Prefix: a.Prefix, Region: a.Region,
		AccessKey: os.Getenv(a.AccessKeyEnv), SecretKey: os.Getenv(a.SecretKeyEnv),
		UseSSL: a.UseSSL, Spool: spool,
	}
}

// BudgetLimits are the daily ceilings by kind plus "global".
func (c Config) BudgetLimits() map[string]float64 {
	out := make(map[string]float64, len(c.Budgets.DailyUSD))
	for k, v := range c.Budgets.DailyUSD {
		out[k] = v
	}
	return out
}

// PostgresDSN resolves database.dsn (a "$NAME" value reads the environment)
// or database.dsn_env. The DSN carries a password, so the file holds a
// variable name and the value stays in the process environment.
func (c Config) PostgresDSN() string {
	v := strings.TrimSpace(c.Database.DSN)
	if after, ok := strings.CutPrefix(v, "$"); ok {
		return os.Getenv(strings.Trim(after, "{}"))
	}
	if v != "" {
		return v
	}
	if c.Database.DSNEnv != "" {
		return os.Getenv(c.Database.DSNEnv)
	}
	return ""
}

// SessionS3Config renders the session section for session.NewS3.
func (c Config) SessionS3Config() session.S3Config {
	s := c.Session
	return session.S3Config{
		Endpoint: s.Endpoint, Bucket: s.Bucket, Prefix: s.Prefix, Region: s.Region,
		AccessKey: os.Getenv(s.AccessKeyEnv), SecretKey: os.Getenv(s.SecretKeyEnv), UseSSL: s.UseSSL,
	}
}

// K8sOptions renders worker.k8s into the Job builder's options.
func (c Config) K8sOptions() k8s.Options {
	w := c.Worker.K8s
	root, err := filepath.Abs(c.DataRoot)
	if err != nil {
		root = c.DataRoot
	}
	return k8s.Options{
		Kubectl: w.Kubectl, Kubeconfig: w.Kubeconfig, Context: w.Context, Namespace: w.Namespace,
		Image: c.Worker.Image, ImagePullPolicy: w.ImagePullPolicy,
		CredentialSecret: w.CredentialSecret, ServiceAccount: w.ServiceAccount,
		DataClaim: w.DataClaim, DataRoot: root,
		NodeSelector: w.NodeSelector, Tolerations: w.Tolerations, Annotations: w.Annotations,
		CPURequest: w.CPURequest, CPULimit: w.CPULimit, MemoryRequest: w.MemoryRequest, MemoryLimit: w.MemoryLimit,
		RunAsUser: w.RunAsUser, FSGroup: w.FSGroup, ReadOnlyRoot: w.ReadOnlyRoot,
		EgressProxy: w.EgressProxy, NoProxy: w.NoProxy,
		TTLAfterFinished: time.Duration(w.TTLSeconds) * time.Second,
		Labels:           map[string]string{"app.kubernetes.io/part-of": "harness"},
	}
}

// K8sStartTimeout bounds waiting for a worker pod to start.
func (c Config) K8sStartTimeout() time.Duration {
	if c.Worker.K8s.StartTimeoutSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.Worker.K8s.StartTimeoutSeconds) * time.Second
}

// ContainerOptions renders worker.docker into the container sandbox options.
func (c Config) ContainerOptions() container.Options {
	d := c.Worker.Docker
	user := d.User
	if user == "host" {
		user = container.UserForHost()
	}
	hostPath := container.PathMap{}
	if to := c.HostDataRoot(); to != "" {
		abs, err := filepath.Abs(c.DataRoot)
		if err != nil {
			abs = c.DataRoot
		}
		hostPath = container.PathMap{From: abs, To: to}
	}
	return container.Options{
		Docker: d.Bin, Image: c.Worker.Image, Network: d.Network, User: user, HostPath: hostPath,
		EgressProxy: d.EgressProxy, NoProxy: d.NoProxy,
		Memory: d.Memory, CPUs: d.CPUs, PidsLimit: d.PidsLimit, ReadOnlyRoot: d.ReadOnlyRoot,
		ExtraArgs: append([]string(nil), d.ExtraArgs...),
	}
}

// HostDataRoot resolves worker.docker.host_data_root. A value starting with
// '$' is read from the harness environment (the same convention as `env:`),
// which is how the compose stack passes the host path into a config file that
// is mounted, and therefore never templated.
func (c Config) HostDataRoot() string {
	v := strings.TrimSpace(c.Worker.Docker.HostDataRoot)
	if after, ok := strings.CutPrefix(v, "$"); ok {
		return os.Getenv(strings.Trim(after, "{}"))
	}
	return v
}

// DockerKillTimeout bounds the container stop loop.
func (c Config) DockerKillTimeout() time.Duration {
	return time.Duration(c.Worker.Docker.KillTimeoutMS) * time.Millisecond
}

// EgressAllow returns the configured allowlist or the built-in default.
func (c Config) EgressAllow() []string {
	if len(c.Egress.Allow) > 0 {
		return c.Egress.Allow
	}
	return egress.DefaultAllow
}

// WorkerEnv renders Env as KEY=VALUE pairs; values starting with '$' are read
// from the harness environment so secrets stay out of the file.
func (c Config) WorkerEnv() []string {
	out := make([]string, 0, len(c.Env))
	for k, v := range c.Env {
		if len(v) > 1 && v[0] == '$' {
			v = os.Getenv(v[1:])
		}
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// repoHolding reports the git repository dataRoot lies inside, if any. It
// walks up from data_root itself, not from the config file: the CLI walks the
// *workspace's* parents for CLAUDE.md and .claude/, so what matters is the
// repository around the checkouts, wherever the config happens to live. A root
// inside any repository silently feeds that repository's instructions to every
// worker. Nothing fails; the runs are just quietly wrong.
func repoHolding(dataRoot string) (string, bool) {
	dir := filepath.Clean(dataRoot)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

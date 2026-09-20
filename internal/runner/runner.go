// Package runner spawns and supervises one `claude -p` process per run
// (design §4.2–§4.4, docs/cli-contract.md).
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/audit"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/session"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// KillReason says why the runner terminated the process group.
type KillReason string

const (
	KillNone     KillReason = ""
	KillTimeout  KillReason = "timeout"
	KillBudget   KillReason = "budget_backstop"
	KillCanceled KillReason = "canceled"
	KillAuth     KillReason = "auth_failed" // api_retry with 401/403: the credential is rejected
)

// Result is everything the evaluator needs from one invocation.
type Result struct {
	// Result is the final event, nil on crash / CLI usage error.
	Result  *events.ResultEvent
	Outcome events.Outcome
	// ExitCode is the process exit status (-1 if it was signalled and no status is known).
	ExitCode int
	Signaled bool
	Stderr   string
	Killed   KillReason
	// EstCostUSD is the token-based running estimate (backstop only).
	EstCostUSD float64
	Events     int
	// SessionLost is set when the CLI reported the transcript missing (cold retry).
	SessionLost bool
	// SessionIDInUse is set when the CLI refused a reused --session-id.
	SessionIDInUse bool
	// AuthFailed is set when the API rejected the worker credential (401/403).
	AuthFailed bool
	// AuthError describes the rejection ("api 401 authentication_failed").
	AuthError string
	// RateLimited is set when the API answered 429 (system/api_retry) or a
	// rate_limit_event reported a non-allowed status; the orchestrator backs
	// off and sheds concurrency (plan Step 18).
	RateLimited bool
	// RateLimitHits counts those signals.
	RateLimitHits int
	// SessionURI is where the transcript snapshot landed ("" if none was written).
	SessionURI  string
	EventLogURI string
	Started     time.Time
	Finished    time.Time
	// ToolUses counts tool_use blocks seen (progress signal).
	ToolUses int
}

// Duration is wall-clock time of the invocation.
func (r *Result) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// Metrics converts the result into the run's stored metrics.
func (r *Result) Metrics() task.Metrics {
	m := task.Metrics{
		DurationMS:     r.Duration().Milliseconds(),
		EstCostUSD:     r.EstCostUSD,
		ExitCode:       r.ExitCode,
		Events:         r.Events,
		TerminalReason: string(r.Outcome),
	}
	if r.Result != nil {
		m.Turns = r.Result.NumTurns
		m.CostUSD = r.Result.TotalCostUSD
		m.DurationAPIMS = r.Result.DurationAPIMS
		if r.Result.DurationMS > 0 {
			m.DurationMS = r.Result.DurationMS
		}
		if r.Result.TerminalReason != "" {
			m.TerminalReason = r.Result.TerminalReason
		}
	}
	return m
}

// Progress receives every decoded event (live UI / Slack thread in Step 16).
type Progress interface {
	OnEvent(runID string, ev *events.Event)
}

// Runner spawns claude processes.
type Runner struct {
	// Bin is the claude executable for the default LocalLauncher; default DefaultBin.
	Bin string
	// Launcher starts the process: nil means LocalLauncher{Bin}; DockerLauncher
	// runs the CLI inside the worker image (plan Step 14).
	Launcher Launcher
	// APIKey is the worker credential (ANTHROPIC_API_KEY). May be empty for
	// fake binaries in tests; a real worker without any credential fails with api_error.
	APIKey string
	// OAuthToken is the alternative credential from `claude setup-token`
	// (CLAUDE_CODE_OAUTH_TOKEN, prefix sk-ant-oat01-). Verified 2026-09-13 to
	// work under an isolated CLAUDE_CONFIG_DIR. It is not an API key: the API
	// rejects it as x-api-key with 401.
	OAuthToken string
	// ExtraEnv are additional allowlisted KEY=VALUE pairs (toolchain vars).
	ExtraEnv []string
	// Args tunes flag construction.
	Args ArgsOptions
	// Grace is the SIGTERM → SIGKILL delay (default 10s).
	Grace time.Duration
	// Prices backs the mid-run cost estimate.
	Prices PriceTable
	// Sessions restores/snapshots transcripts; required.
	Sessions session.Store
	// Audit receives every stdout line; required.
	Audit audit.Sink
	// Progress is optional.
	Progress Progress
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// MaxStderr bounds the captured stderr (default 64 KiB).
	MaxStderr int
}

// Spawn describes where one run executes.
type Spawn struct {
	Workspace string // cwd
	ConfigDir string // empty per-run CLAUDE_CONFIG_DIR
}

// Run executes one invocation. The transcript is snapshotted in a defer, so
// crashes, timeouts and context cancellation still persist it.
func (rn *Runner) Run(ctx context.Context, t *task.Task, r *task.Run, sp Spawn) (res *Result, err error) {
	log := rn.logger().With("run_id", r.ID, "task_id", t.ID, "session_id", r.SessionID, "mode", r.SessionMode)
	if rn.Sessions == nil || rn.Audit == nil {
		return nil, errors.New("runner: Sessions and Audit are required")
	}
	if sp.Workspace == "" || sp.ConfigDir == "" {
		return nil, errors.New("runner: Spawn.Workspace and Spawn.ConfigDir are required")
	}
	if err := os.MkdirAll(sp.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("runner: config dir: %w", err)
	}
	args, err := BuildArgs(t, r, rn.Args)
	if err != nil {
		return nil, err
	}

	// Restore the transcript for continue/fork runs before spawning (§4.4).
	switch r.SessionMode {
	case task.SessionContinue:
		if err := rn.Sessions.Restore(ctx, t.ID, r.SessionID, sp.ConfigDir, sp.Workspace); err != nil {
			if errors.Is(err, session.ErrNotFound) {
				return &Result{Outcome: events.OutcomeCrash, SessionLost: true, ExitCode: -1,
					Stderr: "harness: no transcript snapshot for session " + r.SessionID, Started: time.Now(), Finished: time.Now()}, nil
			}
			return nil, fmt.Errorf("runner: restore session: %w", err)
		}
	case task.SessionFork:
		if err := rn.Sessions.Restore(ctx, t.ID, r.ParentSessionID, sp.ConfigDir, sp.Workspace); err != nil {
			if errors.Is(err, session.ErrNotFound) {
				return &Result{Outcome: events.OutcomeCrash, SessionLost: true, ExitCode: -1,
					Stderr: "harness: no transcript snapshot for parent session " + r.ParentSessionID, Started: time.Now(), Finished: time.Now()}, nil
			}
			return nil, fmt.Errorf("runner: restore parent session: %w", err)
		}
	}

	aw, err := rn.Audit.Open(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("runner: audit: %w", err)
	}
	res = &Result{Started: time.Now(), EventLogURI: aw.URI(), ExitCode: -1}
	defer func() {
		if cerr := aw.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("runner: close audit: %w", cerr)
		}
		res.Finished = time.Now()
		// Snapshot always, even after a crash; ephemeral runs leave nothing.
		if r.SessionMode != task.SessionEphemeral {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			uri, serr := rn.Sessions.Snapshot(sctx, t.ID, r.SessionID, sp.ConfigDir)
			switch {
			case serr == nil:
				res.SessionURI = uri
			case errors.Is(serr, session.ErrNotFound):
				log.Warn("no transcript written by CLI", "config_dir", sp.ConfigDir)
			default:
				log.Error("session snapshot failed", "err", serr)
				if err == nil {
					err = fmt.Errorf("runner: snapshot session: %w", serr)
				}
			}
		}
	}()

	var stdin io.Reader
	if rn.Args.PromptViaStdin {
		stdin = strings.NewReader(t.Prompt)
	}
	launcher := rn.launcher()
	proc, err := launcher.Start(ctx, sp, r.ID, args, rn.env(sp.ConfigDir), stdin)
	if err != nil {
		return res, fmt.Errorf("runner: start (%s): %w", launcher.Name(), err)
	}
	log.Info("worker started", "launcher", launcher.Name(), "worker", proc.ID())
	stdout, stderr := proc.Stdout(), proc.Stderr()

	var (
		killMu   sync.Mutex
		killed   KillReason
		killOnce sync.Once
	)
	kill := func(reason KillReason) {
		killOnce.Do(func() {
			killMu.Lock()
			killed = reason
			killMu.Unlock()
			log.Warn("killing worker", "reason", reason, "worker", proc.ID())
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				log.Warn("SIGTERM failed", "err", err)
			}
			go func() {
				grace := rn.Grace
				if grace <= 0 {
					grace = 10 * time.Second
				}
				time.Sleep(grace)
				_ = proc.Signal(syscall.SIGKILL)
			}()
		})
	}

	// Wall-clock deadline and context cancellation.
	timeout := t.Policy.Timeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.C:
			kill(KillTimeout)
		case <-ctx.Done():
			kill(KillCanceled)
		case <-done:
		}
	}()

	// stderr drain (bounded).
	var errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		limit := rn.MaxStderr
		if limit <= 0 {
			limit = 64 << 10
		}
		_, _ = io.Copy(&limitedBuffer{b: &errBuf, limit: limit}, stderr)
	}()

	// stdout: decode, audit, track cost, detect result.
	tracker := NewCostTracker(rn.prices())
	dec := events.NewDecoder(stdout)
	var decodeErr error
	for {
		ev, derr := dec.Next()
		if derr != nil {
			if !errors.Is(derr, io.EOF) {
				decodeErr = derr
				kill(KillCanceled)
				// Drain so the process can exit.
				_, _ = io.Copy(io.Discard, stdout)
			}
			break
		}
		res.Events++
		if aerr := aw.Append(ev.Raw); aerr != nil {
			log.Error("audit append failed", "err", aerr)
		}
		switch ev.Type {
		case events.TypeSystem:
			if ev.System.IsAuthFailure() {
				res.AuthFailed = true
				res.AuthError = fmt.Sprintf("api %d %s", ev.System.ErrorStatus, ev.System.Error)
				kill(KillAuth)
			}
			if ev.System.IsRateLimited() {
				res.RateLimited = true
				res.RateLimitHits++
			}
		case events.TypeRateLimit:
			if ev.RateLimit.Limited() {
				res.RateLimited = true
				res.RateLimitHits++
			}
		case events.TypeAssistant:
			res.ToolUses += len(ev.Assistant.ToolUses())
			res.EstCostUSD = tracker.Observe(ev.Assistant)
			if res.EstCostUSD > t.Policy.MaxCostUSD {
				kill(KillBudget)
			}
		case events.TypeResult:
			res.Result = ev.Result
		}
		if rn.Progress != nil {
			rn.Progress.OnEvent(r.ID, ev)
		}
	}
	code, signaled, waitErr := proc.Wait()
	close(done)
	wg.Wait()
	res.Stderr = errBuf.String()
	res.EstCostUSD = tracker.Total()

	killMu.Lock()
	res.Killed = killed
	killMu.Unlock()

	if waitErr != nil {
		return res, fmt.Errorf("runner: wait: %w", waitErr)
	}
	res.ExitCode, res.Signaled = code, signaled
	res.Outcome = events.Classify(res.Result)
	if res.AuthFailed {
		res.Outcome = events.OutcomeAPIError
	}
	classifyStderr(res)
	if res.Result != nil && res.Result.SessionID != "" && res.Result.SessionID != r.SessionID && r.SessionMode != task.SessionContinue {
		log.Warn("result session_id differs from minted id", "result_session_id", res.Result.SessionID)
	}
	log.Info("worker exited", "exit_code", res.ExitCode, "outcome", res.Outcome, "killed", res.Killed,
		"events", res.Events, "cost_usd", res.Metrics().CostUSD, "est_cost_usd", res.EstCostUSD, "rate_limited", res.RateLimited)
	if decodeErr != nil {
		return res, fmt.Errorf("runner: decode stdout: %w", decodeErr)
	}
	return res, nil
}

// classifyStderr recognises the one-line fatal CLI errors (fixtures stderr_*.txt).
func classifyStderr(res *Result) {
	s := res.Stderr
	switch {
	case strings.Contains(s, "No conversation found with session ID"):
		res.SessionLost = true
	case strings.Contains(s, "is already in use"):
		res.SessionIDInUse = true
	}
}

// env is the worker allowlist: PATH, CLAUDE_CONFIG_DIR, ANTHROPIC_API_KEY, plus
// ExtraEnv. Nothing else is inherited and HOME is never overridden (it is also
// not passed: the CLI does not need it once CLAUDE_CONFIG_DIR is set; git/ssh
// inside tool calls fall back to the process owner's home via getpwuid).
func (rn *Runner) env(configDir string) []string {
	env := []string{"CLAUDE_CONFIG_DIR=" + configDir}
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}
	// HOME is passed through unchanged so git and ssh inside tool calls find
	// their config; CLAUDE_CONFIG_DIR is what relocates the CLI's own state.
	if h := os.Getenv("HOME"); h != "" {
		env = append(env, "HOME="+h)
	}
	if rn.APIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+rn.APIKey)
	}
	if rn.OAuthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+rn.OAuthToken)
	}
	for _, kv := range rn.ExtraEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "HOME" || k == "CLAUDE_CONFIG_DIR" || k == "ANTHROPIC_API_KEY" || k == "CLAUDE_CODE_OAUTH_TOKEN" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// launcher returns the configured Launcher or a local one for Bin.
func (rn *Runner) launcher() Launcher {
	if rn.Launcher != nil {
		return rn.Launcher
	}
	return &LocalLauncher{Bin: rn.Bin}
}

// Version reports the CLI version the runner would execute (host binary or image).
func (rn *Runner) Version(ctx context.Context) (string, error) { return rn.launcher().Version(ctx) }

func (rn *Runner) prices() PriceTable {
	if rn.Prices.Prices == nil {
		return DefaultPrices
	}
	return rn.Prices
}

func (rn *Runner) logger() *slog.Logger {
	if rn.Logger != nil {
		return rn.Logger
	}
	return slog.Default()
}

type limitedBuffer struct {
	b     *bytes.Buffer
	limit int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.limit - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

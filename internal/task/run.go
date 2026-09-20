package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionMode selects the `claude` session flags for a run (design §4.4).
type SessionMode string

const (
	SessionNew       SessionMode = "new"       // --session-id <new>
	SessionContinue  SessionMode = "continue"  // --resume <task.session_id>
	SessionFork      SessionMode = "fork"      // --resume <parent> --fork-session --session-id <new>
	SessionEphemeral SessionMode = "ephemeral" // --session-id <new> --no-session-persistence
)

// Valid reports whether m is a known mode.
func (m SessionMode) Valid() bool {
	switch m {
	case SessionNew, SessionContinue, SessionFork, SessionEphemeral:
		return true
	}
	return false
}

// Run is one execution attempt of a Task.
type Run struct {
	ID     string `json:"run_id"`
	TaskID string `json:"task_id"`
	// Attempt counts runs within the current phase (1-based); policy.max_retries
	// bounds it per phase.
	Attempt int `json:"attempt"`
	// Phase is the task phase this run executes (0 for single-phase tasks).
	Phase int `json:"phase,omitempty"`
	// Prompt overrides task.Prompt for this run: retry feedback (plan Step 11)
	// or a phase prompt (Step 13). Empty means the task prompt.
	Prompt string `json:"prompt,omitempty"`
	// RetryOf is the run this one retries or follows (previous attempt or phase).
	RetryOf string `json:"retry_of,omitempty"`
	// Output is result.structured_output when the run produced one (a plan).
	Output json.RawMessage `json:"output,omitempty"`
	// SessionID is the id this run passes to the CLI: the minted id for new/fork/ephemeral,
	// the task's resume pointer for continue.
	SessionID   string      `json:"session_id"`
	SessionMode SessionMode `json:"session_mode"`
	// ParentSessionID is the session a fork resumes from. Empty unless SessionMode == fork.
	ParentSessionID string     `json:"parent_session_id,omitempty"`
	SessionURI      string     `json:"session_uri,omitempty"`
	Status          RunStatus  `json:"status"`
	Worker          WorkerInfo `json:"worker"`
	Metrics         Metrics    `json:"metrics"`
	Eval            *Eval      `json:"eval,omitempty"`
	EventLogURI     string     `json:"event_log_uri,omitempty"`
	Artifacts       []string   `json:"artifacts"`
	// LastError is the human-readable reason for the most recent failure or requeue.
	LastError  string     `json:"last_error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// WorkerInfo records where the run executed.
type WorkerInfo struct {
	ContainerID   string `json:"container_id,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	ConfigDir     string `json:"config_dir,omitempty"`
	Branch        string `json:"branch,omitempty"`
}

// Metrics are filled from the result event (authoritative) and the runner.
type Metrics struct {
	Turns         int     `json:"turns"`
	DurationMS    int64   `json:"duration_ms"`
	DurationAPIMS int64   `json:"duration_api_ms,omitempty"`
	CostUSD       float64 `json:"cost_usd"`
	// EstCostUSD is the runner's token-based mid-run estimate (backstop only).
	EstCostUSD float64 `json:"est_cost_usd,omitempty"`
	ExitCode   int     `json:"exit_code"`
	// TerminalReason mirrors result.terminal_reason; "crash" when no result event arrived.
	TerminalReason string `json:"terminal_reason,omitempty"`
	Events         int    `json:"events,omitempty"`
}

// Eval holds the Layer 1 and Layer 2 outputs.
type Eval struct {
	Checks map[string]CheckOutcome `json:"checks"`
	Judge  *JudgeVerdict           `json:"judge,omitempty"`
}

// CheckOutcome is one Layer 1 row.
type CheckOutcome struct {
	Status   string `json:"status"` // pass | fail | n/a
	Evidence string `json:"evidence,omitempty"`
}

// JudgeVerdict is the Layer 2 output (plan Step 9).
type JudgeVerdict struct {
	Verdict     string         `json:"verdict"` // pass | fail | uncertain
	Scores      map[string]int `json:"scores,omitempty"`
	GamedChecks bool           `json:"gamed_checks"`
	Reasoning   string         `json:"reasoning,omitempty"`
	// Samples is how many judge invocations produced the verdict.
	Samples int `json:"samples,omitempty"`
	// CostUSD is the judge spend summed over samples.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Error is set when the judge could not produce a valid verdict (→ uncertain).
	Error string `json:"error,omitempty"`
}

// Judge verdict values.
const (
	VerdictPass      = "pass"
	VerdictFail      = "fail"
	VerdictUncertain = "uncertain"
)

// NewRun builds the first attempt of t as a `new` session. The session id is
// minted here so it exists before the process is spawned (cli-contract #2).
func NewRun(t *Task, now time.Time) *Run {
	r := &Run{
		ID:          NewRunID(),
		TaskID:      t.ID,
		Attempt:     1,
		SessionID:   NewSessionID(),
		SessionMode: SessionNew,
		Status:      StatusQueued,
		Artifacts:   []string{},
		CreatedAt:   now,
	}
	if len(t.Phases) > 0 {
		r.Phase = 1
	}
	return r
}

// ForkRun builds the first attempt of a fan-out child: a fresh session id that
// resumes the planner's transcript with --fork-session, so the child inherits
// the planner's reasoning without either worker writing the other's file
// (design §4.4 fork mode). parentSession is the planner run's session id; the
// snapshot must already be readable under the child's task id (session.Copy).
func ForkRun(t *Task, parentSession string, now time.Time) (*Run, error) {
	if parentSession == "" {
		return nil, errors.New("fork run: no parent session to fork")
	}
	r := &Run{
		ID:              NewRunID(),
		TaskID:          t.ID,
		Attempt:         1,
		SessionID:       NewSessionID(),
		SessionMode:     SessionFork,
		ParentSessionID: parentSession,
		Status:          StatusQueued,
		Artifacts:       []string{},
		CreatedAt:       now,
	}
	if len(t.Phases) > 0 {
		r.Phase = 1
	}
	return r, r.Validate()
}

// NextRun builds the retry of prev (attempt+1, same phase). mode is
// SessionContinue to resume t.SessionID with the failure evidence as prompt
// (design §5.3), or SessionNew for a cold retry after the transcript was lost
// (§9): a fresh id is minted because the CLI refuses reuse (cli-contract #3).
func NextRun(t *Task, prev *Run, mode SessionMode, prompt string, now time.Time) (*Run, error) {
	r := &Run{
		ID:          NewRunID(),
		TaskID:      t.ID,
		Attempt:     prev.Attempt + 1,
		Phase:       prev.Phase,
		Prompt:      prompt,
		RetryOf:     prev.ID,
		SessionMode: mode,
		Status:      StatusQueued,
		Artifacts:   []string{},
		CreatedAt:   now,
	}
	switch mode {
	case SessionContinue:
		if t.SessionID == "" {
			return nil, errors.New("next run: task has no session to resume")
		}
		r.SessionID = t.SessionID
	case SessionNew:
		r.SessionID = NewSessionID()
	default:
		return nil, fmt.Errorf("next run: mode %q not supported (continue or new)", mode)
	}
	return r, nil
}

// RequeueRun builds an operator-triggered retry of a dead run with a **fresh
// attempt counter** (plan Step 18): policy.max_retries applies again from
// scratch, because a human decided the run deserves another chance. The chain
// is still recorded through RetryOf.
func RequeueRun(t *Task, prev *Run, mode SessionMode, prompt string, now time.Time) (*Run, error) {
	r, err := NextRun(t, prev, mode, prompt, now)
	if err != nil {
		return nil, err
	}
	r.Attempt = 1
	return r, nil
}

// PhaseRun builds the first attempt of phase n, resuming the task session so
// the worker keeps the context of the approved phase (design §6 step 3).
func PhaseRun(t *Task, prev *Run, n int, prompt string, now time.Time) (*Run, error) {
	if n < 1 || n > len(t.Phases) {
		return nil, fmt.Errorf("phase run: phase %d out of range 1..%d", n, len(t.Phases))
	}
	if t.SessionID == "" {
		return nil, errors.New("phase run: task has no session to resume")
	}
	return &Run{
		ID:          NewRunID(),
		TaskID:      t.ID,
		Attempt:     1,
		Phase:       n,
		Prompt:      prompt,
		RetryOf:     prev.ID,
		SessionID:   t.SessionID,
		SessionMode: SessionContinue,
		Status:      StatusQueued,
		Artifacts:   []string{},
		CreatedAt:   now,
	}, nil
}

// Validate checks the run's session fields are consistent with its mode.
func (r *Run) Validate() error {
	var errs []error
	if r.ID == "" || len(r.ID) < len(runPrefix) || r.ID[:len(runPrefix)] != runPrefix {
		errs = append(errs, fmt.Errorf("run_id %q must start with %s", r.ID, runPrefix))
	}
	if r.TaskID == "" {
		errs = append(errs, errors.New("task_id is empty"))
	}
	if r.Attempt < 1 {
		errs = append(errs, errors.New("attempt must be >= 1"))
	}
	if !r.SessionMode.Valid() {
		errs = append(errs, fmt.Errorf("session_mode %q unknown", r.SessionMode))
	}
	if r.SessionID == "" {
		errs = append(errs, errors.New("session_id is empty"))
	}
	if r.SessionMode == SessionFork && r.ParentSessionID == "" {
		errs = append(errs, errors.New("fork run needs parent_session_id"))
	}
	if r.SessionMode != SessionFork && r.ParentSessionID != "" {
		errs = append(errs, errors.New("parent_session_id only valid for fork runs"))
	}
	if r.SessionMode == SessionFork && r.ParentSessionID == r.SessionID {
		errs = append(errs, errors.New("fork must mint a new session_id"))
	}
	if !r.Status.Valid() {
		errs = append(errs, fmt.Errorf("status %q unknown", r.Status))
	}
	return errors.Join(errs...)
}

// Package task holds the domain model: immutable task records, per-attempt runs,
// and the run state machine from design §3.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kind is the task template a task is built from.
type Kind string

const (
	KindCodeFix    Kind = "code_fix"
	KindCodeReview Kind = "code_review"
	KindReport     Kind = "report"
	KindTriage     Kind = "triage"
	KindCustom     Kind = "custom"
	// KindCodeFixPlanned is the §6 plan → approve → implement workflow (plan Step 13).
	KindCodeFixPlanned Kind = "code_fix_planned"
	// KindCodeFixFanout is the §6 fan-out workflow (plan Step 21): a planner
	// decomposes the change, one child task per subtask runs in parallel, and
	// a synthesizer merges their results. The parent itself is read-only; the
	// children are ordinary code_fix tasks that each deliver a branch.
	KindCodeFixFanout Kind = "code_fix_fanout"
)

// Kinds lists every known kind.
var Kinds = []Kind{KindCodeFix, KindCodeFixPlanned, KindCodeFixFanout, KindCodeReview, KindReport, KindTriage, KindCustom}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// ChangesWorkspace is true for kinds whose deliverable is a code change,
// so an empty diff is a failure (checks.DiffSanity).
func (k Kind) ChangesWorkspace() bool { return k == KindCodeFix || k == KindCodeFixPlanned }

// Task is an immutable unit of work. Every execution attempt is a Run.
type Task struct {
	ID   string `json:"task_id"`
	Kind Kind   `json:"kind"`
	// Title is a short human label (issue title, cron name). Used for PR and
	// commit titles; the first prompt line is the fallback.
	Title       string        `json:"title,omitempty"`
	Prompt      string        `json:"prompt"`
	Workspace   WorkspaceSpec `json:"workspace"`
	Policy      Policy        `json:"policy"`
	Acceptance  Acceptance    `json:"acceptance"`
	Priority    int           `json:"priority"`
	RequestedBy string        `json:"requested_by"`
	// SessionID is the resume pointer: the session a `continue` run resumes.
	// Set before the first run is spawned; updated from result.session_id after every run.
	SessionID string `json:"session_id,omitempty"`
	// Phase is the phase the task is currently in (1-based) for multi-phase
	// kinds; 0 for single-phase tasks. Updated by the orchestrator when a
	// phase is approved (design §6).
	Phase int `json:"phase,omitempty"`
	// Phases is the multi-phase plan materialised from templates/<kind>/phases/
	// at submission; empty for single-phase tasks. Each entry carries the
	// fully merged policy and acceptance for that phase.
	Phases []PhaseSpec `json:"phases,omitempty"`
	// Child is the template every fan-out child of this task is built from,
	// materialised from templates/<kind>/child/ at submission (plan Step 21).
	// nil for kinds that never fan out.
	Child *ChildSpec `json:"child,omitempty"`
	// ParentID is the fan-out parent whose planner run created this task.
	ParentID string `json:"parent_id,omitempty"`
	// Children are the fan-out children this task's planner run created, in
	// creation order. Written by the store when a child is created, so the
	// parent record names its children without a scan.
	Children  []string  `json:"children,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkspaceSpec describes where the worker runs.
type WorkspaceSpec struct {
	Type        string `json:"type"` // "git"
	Repo        string `json:"repo"` // "org/name", a URL, or a local path (tests)
	Ref         string `json:"ref"`
	WriteAccess bool   `json:"write_access"`
}

// Policy bounds a run. Defaults come from templates/<kind>/policy.json.
type Policy struct {
	AllowedTools []string    `json:"allowed_tools"`
	MaxTurns     int         `json:"max_turns"`
	TimeoutMS    int64       `json:"timeout_ms"`
	MaxCostUSD   float64     `json:"max_cost_usd"`
	MaxRetries   int         `json:"max_retries"`
	Judge        JudgePolicy `json:"judge"`
	// Model overrides the CLI default model when non-empty.
	Model string `json:"model,omitempty"`
	// CriticalTools are tools whose denial makes the run incomplete (checks.PermissionDenials).
	CriticalTools []string `json:"critical_tools,omitempty"`
}

// Timeout returns TimeoutMS as a duration.
func (p Policy) Timeout() time.Duration { return time.Duration(p.TimeoutMS) * time.Millisecond }

// JudgePolicy configures Layer 2.
type JudgePolicy struct {
	Enabled   bool   `json:"enabled"`
	Samples   int    `json:"samples"`
	Threshold int    `json:"threshold"`
	Model     string `json:"model,omitempty"`
}

// Acceptance is the task's machine-checkable definition of done.
type Acceptance struct {
	Commands     []string        `json:"commands"`
	JSONSchema   json.RawMessage `json:"json_schema"`
	DiffScope    []string        `json:"diff_scope"`
	CustomChecks []string        `json:"custom_checks"`
	// ExpectChanges overrides Kind.ChangesWorkspace for checks.DiffSanity:
	// false for a read-only phase of a change kind (a plan), true for a
	// change phase of a read-only kind. nil derives it from the kind.
	ExpectChanges *bool `json:"expect_changes,omitempty"`
}

// HasSchema reports whether the task expects structured output.
func (a Acceptance) HasSchema() bool {
	s := strings.TrimSpace(string(a.JSONSchema))
	return s != "" && s != "null"
}

// Validate checks the record is complete and internally consistent.
func (t *Task) Validate() error {
	var errs []error
	if !strings.HasPrefix(t.ID, taskPrefix) {
		errs = append(errs, fmt.Errorf("task_id %q must start with %s", t.ID, taskPrefix))
	}
	if !t.Kind.Valid() {
		errs = append(errs, fmt.Errorf("kind %q unknown", t.Kind))
	}
	if strings.TrimSpace(t.Prompt) == "" {
		errs = append(errs, errors.New("prompt is empty"))
	}
	if t.Workspace.Type != "git" {
		errs = append(errs, fmt.Errorf("workspace.type %q unsupported (want git)", t.Workspace.Type))
	}
	if t.Workspace.Repo == "" {
		errs = append(errs, errors.New("workspace.repo is empty"))
	}
	if t.Workspace.Ref == "" {
		errs = append(errs, errors.New("workspace.ref is empty"))
	}
	if t.SessionID != "" {
		if _, err := uuid.Parse(t.SessionID); err != nil {
			errs = append(errs, fmt.Errorf("session_id %q is not a UUID", t.SessionID))
		}
	}
	if t.ParentID != "" && !strings.HasPrefix(t.ParentID, taskPrefix) {
		errs = append(errs, fmt.Errorf("parent_id %q must start with %s", t.ParentID, taskPrefix))
	}
	if t.ParentID == t.ID && t.ParentID != "" {
		errs = append(errs, errors.New("parent_id is the task itself"))
	}
	if t.Child != nil {
		if err := t.Child.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := t.Policy.Validate(); err != nil {
		errs = append(errs, err)
	}
	if t.Acceptance.HasSchema() && !json.Valid(t.Acceptance.JSONSchema) {
		errs = append(errs, errors.New("acceptance.json_schema is not valid JSON"))
	}
	if len(t.Phases) > 0 {
		if t.Phase < 1 || t.Phase > len(t.Phases) {
			errs = append(errs, fmt.Errorf("phase %d out of range 1..%d", t.Phase, len(t.Phases)))
		}
		for i, ph := range t.Phases {
			if err := ph.Policy.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("phases[%d] (%s): %w", i, ph.Name, err))
			}
			if ph.Acceptance.HasSchema() && !json.Valid(ph.Acceptance.JSONSchema) {
				errs = append(errs, fmt.Errorf("phases[%d] (%s): acceptance.json_schema is not valid JSON", i, ph.Name))
			}
			if i > 0 && strings.TrimSpace(ph.Prompt) == "" {
				errs = append(errs, fmt.Errorf("phases[%d] (%s): prompt is empty (phases after the first need one)", i, ph.Name))
			}
		}
	} else if t.Phase != 0 {
		errs = append(errs, errors.New("phase set on a single-phase task"))
	}
	return errors.Join(errs...)
}

// ExpectsChanges reports whether the effective task must change the workspace
// (checks.DiffSanity). Acceptance.ExpectChanges wins over the kind default.
func (t *Task) ExpectsChanges() bool {
	if t.Acceptance.ExpectChanges != nil {
		return *t.Acceptance.ExpectChanges
	}
	return t.Kind.ChangesWorkspace()
}

// Validate checks the bounds are positive and the tool allowlist is present.
func (p Policy) Validate() error {
	var errs []error
	if len(p.AllowedTools) == 0 {
		errs = append(errs, errors.New("policy.allowed_tools is empty"))
	}
	for _, tool := range p.AllowedTools {
		if strings.ContainsAny(tool, ",\n") {
			errs = append(errs, fmt.Errorf("policy.allowed_tools entry %q contains a separator", tool))
		}
	}
	if p.MaxTurns <= 0 {
		errs = append(errs, errors.New("policy.max_turns must be > 0"))
	}
	if p.TimeoutMS <= 0 {
		errs = append(errs, errors.New("policy.timeout_ms must be > 0"))
	}
	if p.MaxCostUSD <= 0 {
		errs = append(errs, errors.New("policy.max_cost_usd must be > 0"))
	}
	if p.MaxRetries < 0 {
		errs = append(errs, errors.New("policy.max_retries must be >= 0"))
	}
	if p.Judge.Enabled {
		if p.Judge.Samples <= 0 {
			errs = append(errs, errors.New("policy.judge.samples must be > 0 when the judge is enabled"))
		}
		if p.Judge.Threshold < 0 || p.Judge.Threshold > 10 {
			errs = append(errs, errors.New("policy.judge.threshold must be in 0..10"))
		}
	}
	return errors.Join(errs...)
}

// Merge returns base with every field present in override applied on top.
// override is a JSON object; absent keys keep the base value, present keys
// replace it (slices are replaced whole, nested objects merge key by key).
// The result shares no memory with p: decoding into a copy of the struct
// would otherwise write through the copied slice headers into p's arrays.
func (p Policy) Merge(override json.RawMessage) (Policy, error) {
	var out Policy
	if err := deepCopy(p, &out); err != nil {
		return p, fmt.Errorf("merge policy: %w", err)
	}
	if len(override) == 0 || string(override) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(override, &out); err != nil {
		return p, fmt.Errorf("merge policy override: %w", err)
	}
	return out, nil
}

// Merge applies a partial acceptance object on top of a (see Policy.Merge).
func (a Acceptance) Merge(override json.RawMessage) (Acceptance, error) {
	var out Acceptance
	if err := deepCopy(a, &out); err != nil {
		return a, fmt.Errorf("merge acceptance: %w", err)
	}
	if len(override) == 0 || string(override) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(override, &out); err != nil {
		return a, fmt.Errorf("merge acceptance override: %w", err)
	}
	return out, nil
}

// deepCopy clones src into dst through JSON so no slice or RawMessage is shared.
func deepCopy(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

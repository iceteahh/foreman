// Package golden is Layer 4 of the evaluation pipeline (design §5.4): the
// offline suite that evaluates the harness itself. A case pairs a task with a
// fixture repository and the outcome a human already agreed is correct; the
// suite replays every case through the real pipeline and scores the result.
//
// Nothing here spawns an LLM: the suite drives the orchestrator through the
// Executor seam, so scoring, reporting and gating are pure and testable
// without spending a token.
package golden

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// Outcome is the coarse verdict a case expects, and what a finished run
// produced. It is deliberately coarser than task.RunStatus: the suite asserts
// "this task should be delivered / should not be delivered / should stop for a
// human", not which status edge the orchestrator happened to take.
type Outcome string

const (
	// OutcomePass means the run was delivered (or passed with no adapter).
	OutcomePass Outcome = "pass"
	// OutcomeFail means the run exhausted its retries (dead) or ended failed.
	OutcomeFail Outcome = "fail"
	// OutcomeNeedsReview means routing stopped for a human.
	OutcomeNeedsReview Outcome = "needs_review"
	// OutcomeError means the harness itself could not finish the case
	// (provisioning, spawn, timeout). Never a valid expectation.
	OutcomeError Outcome = "error"
)

// Outcomes lists the expectations a case may declare.
var Outcomes = []Outcome{OutcomePass, OutcomeFail, OutcomeNeedsReview}

// Valid reports whether o is an expectation a case may declare.
func (o Outcome) Valid() bool {
	for _, x := range Outcomes {
		if x == o {
			return true
		}
	}
	return false
}

// OutcomeOf maps a finished run onto the coarse verdict.
func OutcomeOf(r *task.Run) Outcome {
	if r == nil {
		return OutcomeError
	}
	switch r.Status {
	case task.StatusDelivered, task.StatusPassed:
		return OutcomePass
	case task.StatusDead, task.StatusFailed, task.StatusClosed:
		return OutcomeFail
	case task.StatusNeedsReview:
		return OutcomeNeedsReview
	default:
		return OutcomeError
	}
}

// Case is one `evals/golden/<id>.json` document.
type Case struct {
	// ID is the stable case name; it defaults to the file's base name and
	// must stay stable, because the regression gate compares reports by id.
	ID string `json:"id,omitempty"`
	// Description says what behaviour the case pins down. Written for the
	// person who has to judge whether a future regression is acceptable.
	Description string    `json:"description"`
	Kind        task.Kind `json:"kind"`
	Title       string    `json:"title,omitempty"`
	Prompt      string    `json:"prompt"`
	// Policy and Acceptance are partial overrides on the kind template, the
	// same shape a POST /tasks body uses. Cases normally pin the model and a
	// small cost ceiling so the suite is cheap and reproducible.
	Policy     json.RawMessage `json:"policy,omitempty"`
	Acceptance json.RawMessage `json:"acceptance,omitempty"`
	Repo       Repo            `json:"repo"`
	Expect     Expect          `json:"expect"`
	// Tags select subsets (`harness eval run -tag cheap`).
	Tags []string `json:"tags,omitempty"`
	// Skip, when non-empty, records why the case is not executed. It still
	// appears in the report so a silently dropped case is visible.
	Skip string `json:"skip,omitempty"`

	// Path is the file the case was loaded from (not serialised).
	Path string `json:"-"`
}

// Repo is the fixture repository a case runs against. Exactly one of Bundle
// and Dir is set.
//
// Bundle is the design's shape (§5.4: "fixture repo as a git bundle") and is
// what a case captured from real history uses. Dir is a plain directory tree
// committed as one initial commit; it is preferred for hand-written cases
// because a reviewer can read and diff the fixture in the pull request.
type Repo struct {
	// Bundle is a `git bundle` file, relative to the case file.
	Bundle string `json:"bundle,omitempty"`
	// Dir is a directory tree, relative to the case file, committed as the
	// initial commit of a fresh repository.
	Dir string `json:"dir,omitempty"`
	// Ref is the branch the task starts from (default "main").
	Ref string `json:"ref,omitempty"`
}

// Branch returns the fixture branch, defaulting to main.
func (r Repo) Branch() string {
	if r.Ref != "" {
		return r.Ref
	}
	return "main"
}

// Expect is the known-good outcome of a case.
type Expect struct {
	Outcome Outcome `json:"outcome"`
	// FilesTouched are globs (doublestar) that must each match at least one
	// changed file.
	FilesTouched []string `json:"files_touched,omitempty"`
	// FilesUntouched are globs no changed file may match. Use it to pin the
	// "did not wander" property that a diff_scope alone does not catch.
	FilesUntouched []string `json:"files_untouched,omitempty"`
	// MaxAttempts bounds how many runs the task may take (0 = unbounded).
	// A case that starts needing retries is a regression even when it ends up
	// delivering, so most cases set it.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// JudgeVerdict, when set, asserts what Layer 2 concluded.
	JudgeVerdict string `json:"judge_verdict,omitempty"`
	// OutputSchema, when set, validates the run's structured_output. It is the
	// deliverable of the read-only kinds, which change no files.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

// Spec renders the case as a task submission. repo is the materialised
// fixture origin (see Fixtures.Materialize).
func (c *Case) Spec(repo string) task.Spec {
	return task.Spec{
		Kind:        c.Kind,
		Title:       c.Title,
		Prompt:      c.Prompt,
		Workspace:   task.WorkspaceSpec{Type: "git", Repo: repo, Ref: c.Repo.Branch()},
		Policy:      c.Policy,
		Acceptance:  c.Acceptance,
		RequestedBy: "golden:" + c.ID,
	}
}

// HasTag reports whether the case carries tag.
func (c *Case) HasTag(tag string) bool {
	for _, t := range c.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// Validate checks the case is complete and internally consistent. It runs at
// load time so a malformed case fails the suite instead of scoring as a
// regression.
func (c *Case) Validate() error {
	var errs []error
	if strings.TrimSpace(c.ID) == "" {
		errs = append(errs, errors.New("id is empty"))
	}
	if strings.TrimSpace(c.Description) == "" {
		errs = append(errs, errors.New("description is empty (say what behaviour the case pins down)"))
	}
	if !c.Kind.Valid() {
		errs = append(errs, fmt.Errorf("kind %q unknown", c.Kind))
	}
	if strings.TrimSpace(c.Prompt) == "" {
		errs = append(errs, errors.New("prompt is empty"))
	}
	switch {
	case c.Repo.Bundle == "" && c.Repo.Dir == "":
		errs = append(errs, errors.New("repo needs either bundle or dir"))
	case c.Repo.Bundle != "" && c.Repo.Dir != "":
		errs = append(errs, errors.New("repo sets both bundle and dir; pick one"))
	}
	for _, p := range []string{c.Repo.Bundle, c.Repo.Dir} {
		if p != "" && (filepath.IsAbs(p) || strings.HasPrefix(filepath.ToSlash(p), "../")) {
			errs = append(errs, fmt.Errorf("repo path %q must be relative to the case file and stay inside the suite", p))
		}
	}
	if !c.Expect.Outcome.Valid() {
		errs = append(errs, fmt.Errorf("expect.outcome %q must be one of %v", c.Expect.Outcome, Outcomes))
	}
	if v := c.Expect.JudgeVerdict; v != "" && v != task.VerdictPass && v != task.VerdictFail && v != task.VerdictUncertain {
		errs = append(errs, fmt.Errorf("expect.judge_verdict %q must be pass, fail or uncertain", v))
	}
	if c.Expect.MaxAttempts < 0 {
		errs = append(errs, errors.New("expect.max_attempts must be >= 0"))
	}
	if len(c.Expect.OutputSchema) > 0 && !json.Valid(c.Expect.OutputSchema) {
		errs = append(errs, errors.New("expect.output_schema is not valid JSON"))
	}
	if c.Expect.Outcome != OutcomePass && len(c.Expect.FilesTouched) > 0 {
		errs = append(errs, errors.New("expect.files_touched only applies to a pass outcome"))
	}
	// The overrides must parse now, not when the suite is halfway through a
	// paid run.
	if _, err := (task.Policy{}).Merge(c.Policy); err != nil && len(c.Policy) > 0 {
		errs = append(errs, fmt.Errorf("policy override: %w", err))
	}
	if _, err := (task.Acceptance{}).Merge(c.Acceptance); err != nil && len(c.Acceptance) > 0 {
		errs = append(errs, fmt.Errorf("acceptance override: %w", err))
	}
	return errors.Join(errs...)
}

// Suite is a loaded directory of cases, ordered by id.
type Suite struct {
	Dir   string
	Cases []*Case
}

// IDs lists every case id in order.
func (s Suite) IDs() []string {
	out := make([]string, 0, len(s.Cases))
	for _, c := range s.Cases {
		out = append(out, c.ID)
	}
	return out
}

// Get returns the case with the given id.
func (s Suite) Get(id string) (*Case, bool) {
	for _, c := range s.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return nil, false
}

// Sort orders the cases by id so reports are stable.
func (s *Suite) Sort() {
	sort.Slice(s.Cases, func(i, j int) bool { return s.Cases[i].ID < s.Cases[j].ID })
}

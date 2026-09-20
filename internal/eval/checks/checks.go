// Package checks is Layer 1 of the evaluation pipeline: deterministic,
// zero-AI checks that run in the workspace after the worker exits (design §5.1).
package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// Status of one check.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
	NA   Status = "n/a"
)

// Result is one row of the report. Evidence is verbatim text that becomes
// retry feedback (plan Step 11), so it must be specific.
type Result struct {
	Name     string
	Status   Status
	Evidence string
}

// Report is the Layer 1 output.
type Report struct {
	Results []Result
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Results {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Get returns the named result.
func (r Report) Get(name string) (Result, bool) {
	for _, c := range r.Results {
		if c.Name == name {
			return c, true
		}
	}
	return Result{}, false
}

// Outcomes converts the report into the run's stored shape.
func (r Report) Outcomes() map[string]task.CheckOutcome {
	out := make(map[string]task.CheckOutcome, len(r.Results))
	for _, c := range r.Results {
		out[c.Name] = task.CheckOutcome{Status: string(c.Status), Evidence: c.Evidence}
	}
	return out
}

// FromOutcomes rebuilds a Report from the stored run.eval.checks map, in the
// Default() order with unknown names appended alphabetically.
func FromOutcomes(m map[string]task.CheckOutcome) Report {
	var rep Report
	seen := map[string]bool{}
	for _, c := range Default() {
		if o, ok := m[c.Name()]; ok {
			rep.Results = append(rep.Results, Result{Name: c.Name(), Status: Status(o.Status), Evidence: o.Evidence})
			seen[c.Name()] = true
		}
	}
	var rest []string
	for name := range m {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		rep.Results = append(rep.Results, Result{Name: name, Status: Status(m[name].Status), Evidence: m[name].Evidence})
	}
	return rep
}

// FailedNames lists the failing checks.
func (r Report) FailedNames() []string {
	var out []string
	for _, c := range r.Results {
		if c.Status == Fail {
			out = append(out, c.Name)
		}
	}
	return out
}

// Summary is a one-line "pass: a, b · fail: c · n/a: d".
func (r Report) Summary() string {
	groups := map[Status][]string{}
	for _, c := range r.Results {
		groups[c.Status] = append(groups[c.Status], c.Name)
	}
	var parts []string
	for _, s := range []Status{Pass, Fail, NA} {
		if names := groups[s]; len(names) > 0 {
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("%s: %s", s, strings.Join(names, ", ")))
		}
	}
	return strings.Join(parts, " · ")
}

// Markdown renders the report as a table plus evidence blocks (PR body, feedback).
func (r Report) Markdown() string {
	var sb strings.Builder
	sb.WriteString("| Check | Status |\n|---|---|\n")
	for _, c := range r.Results {
		fmt.Fprintf(&sb, "| %s | %s |\n", c.Name, statusEmoji(c.Status))
	}
	for _, c := range r.Results {
		if c.Evidence == "" || c.Status == Pass {
			continue
		}
		fmt.Fprintf(&sb, "\n<details><summary><code>%s</code> — %s</summary>\n\n```\n%s\n```\n</details>\n", c.Name, c.Status, strings.TrimSpace(c.Evidence))
	}
	return sb.String()
}

func statusEmoji(s Status) string {
	switch s {
	case Pass:
		return "✅ pass"
	case Fail:
		return "❌ fail"
	default:
		return "➖ n/a"
	}
}

// Input is everything a check may look at. Checks never see the worker transcript.
type Input struct {
	Task      *task.Task
	Run       *task.Run
	Result    *runner.Result
	Workspace workspace.Workspace
	// Capture is filled by Run when nil.
	Capture *workspace.Capture
	// CommandTimeout bounds each acceptance command (default 10m).
	CommandTimeout time.Duration
	// Outputs receives acceptance command output keyed by command (for artifacts/feedback).
	Outputs map[string]workspace.ExecResult
}

// Check is one deterministic rule.
type Check interface {
	Name() string
	Run(ctx context.Context, in *Input) Result
}

// Run executes checks in order and returns the report. It never stops early:
// every row is useful feedback.
func Run(ctx context.Context, in *Input, checks ...Check) (Report, error) {
	if in.Capture == nil && in.Workspace != nil {
		c, err := in.Workspace.Capture(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("checks: capture workspace: %w", err)
		}
		in.Capture = c
	}
	if in.Outputs == nil {
		in.Outputs = map[string]workspace.ExecResult{}
	}
	var rep Report
	for _, c := range checks {
		res := c.Run(ctx, in)
		res.Name = c.Name()
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

// Default returns every §5.1 check in table order.
func Default() []Check {
	return []Check{ExitCode{}, TurnExhaustion{}, PermissionDenials{}, AcceptanceCommands{}, DiffScope{}, DiffSanity{}, SchemaValidation{}}
}

func pass(evidence string) Result { return Result{Status: Pass, Evidence: evidence} }
func fail(evidence string) Result { return Result{Status: Fail, Evidence: evidence} }
func na(evidence string) Result   { return Result{Status: NA, Evidence: evidence} }

// tail keeps the last n bytes of s for evidence.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

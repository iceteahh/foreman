package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
)

// ExitCode: process health. Fails on a missing result (crash), a non-zero exit
// paired with is_error / terminal_reason != completed, or any kill.
type ExitCode struct{}

func (ExitCode) Name() string { return "exit_code" }

func (ExitCode) Run(_ context.Context, in *Input) Result {
	r := in.Result
	if r == nil {
		return fail("runner produced no result")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code=%d outcome=%s", r.ExitCode, r.Outcome)
	if r.Killed != "" {
		fmt.Fprintf(&sb, " killed=%s", r.Killed)
	}
	if r.Result != nil {
		fmt.Fprintf(&sb, " is_error=%v terminal_reason=%s subtype=%s", r.Result.IsError, r.Result.TerminalReason, r.Result.Subtype)
		if len(r.Result.Errors) > 0 {
			fmt.Fprintf(&sb, " errors=%q", r.Result.Errors)
		}
	}
	if s := tail(r.Stderr, 2000); s != "" {
		fmt.Fprintf(&sb, "\nstderr:\n%s", s)
	}
	switch {
	case r.Result == nil:
		sb.WriteString("\nprocess exited without a result event (crash, timeout, or CLI usage error)")
		return fail(sb.String())
	case r.Killed != "":
		return fail(sb.String())
	case r.Outcome == events.OutcomeCompleted && !r.Result.IsError && r.ExitCode == 0:
		return pass(sb.String())
	case r.Outcome == events.OutcomeCompleted && !r.Result.IsError && r.ExitCode != 0:
		// The CLI reported completion but exited non-zero: treat as unhealthy.
		return fail(sb.String())
	default:
		return fail(sb.String())
	}
}

// TurnExhaustion: result.num_turns reached max_turns (counted per invocation).
type TurnExhaustion struct{}

func (TurnExhaustion) Name() string { return "turn_exhaustion" }

func (TurnExhaustion) Run(_ context.Context, in *Input) Result {
	if in.Result == nil || in.Result.Result == nil {
		return na("no result event")
	}
	turns, max := in.Result.Result.NumTurns, in.Task.Policy.MaxTurns
	ev := fmt.Sprintf("num_turns=%d max_turns=%d", turns, max)
	if in.Result.Outcome == events.OutcomeMaxTurns || (max > 0 && turns >= max) {
		return fail(ev + "\nthe worker used every allowed turn; it was probably stuck")
	}
	return pass(ev)
}

// PermissionDenials: a denial on a task-critical tool means the run is incomplete.
type PermissionDenials struct{}

func (PermissionDenials) Name() string { return "permission_denials" }

func (PermissionDenials) Run(_ context.Context, in *Input) Result {
	if in.Result == nil || in.Result.Result == nil {
		return na("no result event")
	}
	denials := in.Result.Result.PermissionDenials
	if len(denials) == 0 {
		return pass("no permission denials")
	}
	var lines, critical []string
	for _, d := range denials {
		line := d.Tool
		if len(d.Input) > 0 {
			line += " " + tail(string(d.Input), 200)
		}
		lines = append(lines, line)
		if isCritical(d.Tool, in.Task.Policy.CriticalTools) {
			critical = append(critical, d.Tool)
		}
	}
	ev := fmt.Sprintf("%d denial(s):\n%s", len(denials), strings.Join(lines, "\n"))
	if len(critical) > 0 {
		return fail(ev + "\ndenied task-critical tool(s): " + strings.Join(critical, ", "))
	}
	return pass(ev + "\nno task-critical tool was denied")
}

// isCritical matches a denied tool name against the policy's critical list.
// "Bash" in the list matches any "Bash(…)" pattern and vice versa.
func isCritical(tool string, critical []string) bool {
	base := func(s string) string {
		if i := strings.Index(s, "("); i > 0 {
			return s[:i]
		}
		return s
	}
	for _, c := range critical {
		if c == tool || base(c) == base(tool) {
			return true
		}
	}
	return false
}

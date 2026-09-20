package runner

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// ArgsOptions tune the flags that are not part of the task policy.
type ArgsOptions struct {
	// BudgetFlagRatio scales policy.max_cost_usd into --max-budget-usd
	// (the CLI checks the cap after the turn that crosses it). Default 0.9.
	BudgetFlagRatio float64
	// PromptViaStdin omits the prompt from argv; the runner writes it to stdin.
	PromptViaStdin bool
	// Bare uses --bare instead of --setting-sources project --strict-mcp-config
	// (plan open decision 5). Off until verified.
	Bare bool
	// AddDirs are passed as --add-dir (used with Bare to re-enable workspace CLAUDE.md).
	AddDirs []string
	// MCPConfig, when set, is passed as --mcp-config alongside --strict-mcp-config.
	MCPConfig string
	// NoVerbose drops --verbose (kept on by default so stream-json includes every event).
	NoVerbose bool
}

// DefaultBudgetFlagRatio is used when ArgsOptions.BudgetFlagRatio is zero.
const DefaultBudgetFlagRatio = 0.9

// ErrIllegalSession is wrapped by BuildArgs for session flag conflicts.
var ErrIllegalSession = errors.New("illegal session configuration")

// BuildArgs is a pure function producing the argv (without the executable) for
// `claude -p` per docs/cli-contract.md §2. The prompt is a single argv element
// (or stdin); nothing is ever shell-interpolated.
func BuildArgs(t *task.Task, r *task.Run, o ArgsOptions) ([]string, error) {
	if err := t.Policy.Validate(); err != nil {
		return nil, err
	}
	ratio := o.BudgetFlagRatio
	if ratio == 0 {
		ratio = DefaultBudgetFlagRatio
	}
	if ratio <= 0 || ratio > 1 {
		return nil, fmt.Errorf("budget flag ratio %v must be in (0,1]", ratio)
	}
	sess, err := sessionArgs(r)
	if err != nil {
		return nil, err
	}

	args := []string{"-p"}
	if !o.PromptViaStdin {
		args = append(args, t.Prompt)
	}
	args = append(args, "--output-format", "stream-json")
	if !o.NoVerbose {
		args = append(args, "--verbose")
	}
	args = append(args, "--include-partial-messages",
		"--allowedTools", strings.Join(t.Policy.AllowedTools, ","),
		"--max-turns", strconv.Itoa(t.Policy.MaxTurns),
		"--max-budget-usd", formatUSD(t.Policy.MaxCostUSD*ratio),
		"--permission-mode", "dontAsk",
	)
	if o.Bare {
		args = append(args, "--bare")
		for _, d := range o.AddDirs {
			args = append(args, "--add-dir", d)
		}
	} else {
		args = append(args, "--setting-sources", "project", "--strict-mcp-config")
	}
	if o.MCPConfig != "" {
		args = append(args, "--mcp-config", o.MCPConfig)
	}
	if t.Acceptance.HasSchema() {
		args = append(args, "--json-schema", string(t.Acceptance.JSONSchema))
	}
	if t.Policy.Model != "" {
		args = append(args, "--model", t.Policy.Model)
	}
	args = append(args, sess...)
	return args, nil
}

// sessionArgs encodes design §4.4: --session-id XOR --resume unless forking.
func sessionArgs(r *task.Run) ([]string, error) {
	if _, err := uuid.Parse(r.SessionID); err != nil {
		return nil, fmt.Errorf("%w: session_id %q is not a UUID", ErrIllegalSession, r.SessionID)
	}
	switch r.SessionMode {
	case task.SessionNew:
		if r.ParentSessionID != "" {
			return nil, fmt.Errorf("%w: new session with parent_session_id", ErrIllegalSession)
		}
		return []string{"--session-id", r.SessionID}, nil
	case task.SessionContinue:
		if r.ParentSessionID != "" {
			return nil, fmt.Errorf("%w: continue with parent_session_id (use fork)", ErrIllegalSession)
		}
		return []string{"--resume", r.SessionID}, nil
	case task.SessionFork:
		if r.ParentSessionID == "" {
			return nil, fmt.Errorf("%w: fork without parent_session_id", ErrIllegalSession)
		}
		if _, err := uuid.Parse(r.ParentSessionID); err != nil {
			return nil, fmt.Errorf("%w: parent_session_id %q is not a UUID", ErrIllegalSession, r.ParentSessionID)
		}
		if r.ParentSessionID == r.SessionID {
			return nil, fmt.Errorf("%w: fork reuses the parent session id", ErrIllegalSession)
		}
		return []string{"--resume", r.ParentSessionID, "--fork-session", "--session-id", r.SessionID}, nil
	case task.SessionEphemeral:
		return []string{"--session-id", r.SessionID, "--no-session-persistence"}, nil
	}
	return nil, fmt.Errorf("%w: unknown session mode %q", ErrIllegalSession, r.SessionMode)
}

func formatUSD(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

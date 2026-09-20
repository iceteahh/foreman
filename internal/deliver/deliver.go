// Package deliver ships a passed run to its production surface (design §2.1,
// §9 idempotency). Adapters are idempotent: a retried delivery updates, never duplicates.
package deliver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

// Input is what an adapter gets.
type Input struct {
	Task      *task.Task
	Run       *task.Run
	Workspace workspace.Workspace
	Checks    checks.Report
	Result    *runner.Result
}

// Adapter delivers and returns artifact identifiers ("pr:org/repo#12", "branch:harness/tsk_…@sha").
type Adapter interface {
	Name() string
	Deliver(ctx context.Context, in *Input) ([]string, error)
}

// Subject is the short human label: task.Title, else the prompt's first line.
func Subject(in *Input, max int) string {
	if t := strings.TrimSpace(in.Task.Title); t != "" {
		return truncate(t, max)
	}
	return firstLine(in.Task.Prompt, max)
}

// CommitMessage renders the bot commit message.
func CommitMessage(in *Input) string {
	return fmt.Sprintf("harness: %s\n\nTask: %s\nRun: %s (attempt %d)\nRequested by: %s\n",
		Subject(in, 60), in.Task.ID, in.Run.ID, in.Run.Attempt, in.Task.RequestedBy)
}

// Title renders the PR title.
func Title(in *Input) string { return "[harness] " + Subject(in, 70) }

var webhookIssueRe = regexp.MustCompile(`^webhook:github:([^#\s]+)#(\d+)$`)

// closesLine returns "Closes #N" when the task came from an issue in the same repo.
func closesLine(in *Input) string {
	m := webhookIssueRe.FindStringSubmatch(in.Task.RequestedBy)
	if m == nil || !strings.EqualFold(strings.TrimSuffix(m[1], ".git"), strings.TrimSuffix(in.Task.Workspace.Repo, ".git")) {
		return ""
	}
	return "Closes #" + m[2]
}

// Body renders the PR body with the check report.
func Body(in *Input) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Automated change by the agent harness.\n\n")
	if c := closesLine(in); c != "" {
		fmt.Fprintf(&sb, "%s\n\n", c)
	}
	fmt.Fprintf(&sb, "| | |\n|---|---|\n| Task | `%s` |\n| Run | `%s` (attempt %d) |\n| Requested by | %s |\n", in.Task.ID, in.Run.ID, in.Run.Attempt, in.Task.RequestedBy)
	if in.Result != nil {
		m := in.Result.Metrics()
		fmt.Fprintf(&sb, "| Turns | %d |\n| Cost | $%.4f |\n| Duration | %ds |\n", m.Turns, m.CostUSD, m.DurationMS/1000)
	}
	sb.WriteString("\n## Deterministic checks\n\n")
	sb.WriteString(in.Checks.Markdown())
	if in.Result != nil && in.Result.Result != nil {
		if txt := strings.TrimSpace(in.Result.Result.Text()); txt != "" {
			fmt.Fprintf(&sb, "\n## Worker summary\n\n%s\n", truncate(txt, 6000))
		}
	}
	sb.WriteString("\n---\n_Review before merging: the harness never merges to protected branches._\n")
	return sb.String()
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimLeft(s, "# ")
	return truncate(s, max)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

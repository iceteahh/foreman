// Package feedback renders the retry prompt (design §5.3, plan Step 11): the
// failure evidence verbatim — check logs and judge reasoning — followed by
// "fix these issues". Resumed into the same session, the worker repairs
// instead of restarting; for a cold retry the original task is prepended.
package feedback

import (
	"fmt"
	"sort"
	"strings"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/judge"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// MaxEvidence bounds each check's evidence block in the prompt.
const MaxEvidence = 8000

// Render builds the feedback for a failed attempt. attempt is the attempt that
// failed; judge may be nil (Layer 2 skipped) and comment is a human reviewer's
// note (empty unless the retry comes from a rejection).
func Render(attempt int, rep checks.Report, jv *judge.Verdict, comment string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Attempt %d of this task was evaluated and rejected. Fix the issues below in this workspace, then run the tests again. Do not start over: build on the work you already did.\n", attempt)

	if failed := rep.FailedNames(); len(failed) > 0 {
		sb.WriteString("\n## Failed deterministic checks\n")
		for _, c := range rep.Results {
			if c.Status != checks.Fail {
				continue
			}
			fmt.Fprintf(&sb, "\n### %s\n", c.Name)
			if ev := strings.TrimSpace(c.Evidence); ev != "" {
				fmt.Fprintf(&sb, "```\n%s\n```\n", clip(ev, MaxEvidence))
			}
		}
	}
	if jv != nil && (jv.Failed() || jv.Verdict == task.VerdictUncertain) {
		fmt.Fprintf(&sb, "\n## Independent review (verdict: %s", jv.Verdict)
		if jv.GamedChecks {
			sb.WriteString(", checks were gamed")
		}
		sb.WriteString(")\n")
		if len(jv.Scores) > 0 {
			keys := make([]string, 0, len(jv.Scores))
			for k := range jv.Scores {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%d", k, jv.Scores[k]))
			}
			fmt.Fprintf(&sb, "Scores (0–10): %s\n", strings.Join(parts, ", "))
		}
		if r := strings.TrimSpace(jv.Reasoning); r != "" {
			fmt.Fprintf(&sb, "\n%s\n", clip(r, MaxEvidence))
		}
	}
	if c := strings.TrimSpace(comment); c != "" {
		fmt.Fprintf(&sb, "\n## Reviewer's note\n%s\n", clip(c, MaxEvidence))
	}
	sb.WriteString("\n## What to do\n- Fix these issues; do not weaken, skip or delete tests to make checks pass.\n- Keep the change minimal and in scope.\n- Run the repository's test and lint commands before you finish.\n- Do not commit or push. The harness captures your working tree.\n")
	return sb.String()
}

// Cold prefixes the original task so a fresh session (transcript lost) has
// the full context.
func Cold(taskPrompt, fb string) string {
	return strings.TrimSpace(taskPrompt) + "\n\n---\n\nA previous attempt at this task was made in a session whose transcript is no longer available. Its evaluation follows; use it to avoid the same mistakes.\n\n" + fb
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

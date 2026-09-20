package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
)

var promptTmpl = template.Must(template.New("judge").Option("missingkey=error").Parse(string(mustRead("prompt.md.tmpl"))))

type promptData struct {
	Kind, Prompt     string
	Commands         []string
	DiffScope        []string
	ExpectChanges    bool
	HasSchema        bool
	ChecksSummary    string
	FailedChecks     []checks.Result
	AcceptanceOutput string
	BaseSHA          string
	ChangedCount     int
	Diff             string
	DiffTruncated    bool
	FinalMessage     string
	StructuredOutput string
}

// Prompt renders the judge prompt from the input. It contains the task spec,
// the diff, acceptance outputs and the check summary — never the transcript.
func Prompt(in *Input, maxDiff int) (string, error) {
	d := promptData{
		Kind: string(in.Task.Kind), Prompt: strings.TrimSpace(in.Task.Prompt),
		Commands: in.Task.Acceptance.Commands, DiffScope: in.Task.Acceptance.DiffScope,
		ExpectChanges: in.Task.ExpectsChanges(), HasSchema: in.Task.Acceptance.HasSchema(),
		ChecksSummary: in.Checks.Summary(),
	}
	if d.ChecksSummary == "" {
		d.ChecksSummary = "(no deterministic checks ran)"
	}
	for _, c := range in.Checks.Results {
		if c.Status == checks.Fail && c.Evidence != "" {
			d.FailedChecks = append(d.FailedChecks, checks.Result{Name: c.Name, Evidence: clip(c.Evidence, 4000)})
		}
	}
	if len(in.Outputs) > 0 {
		var sb strings.Builder
		for _, cmd := range sortedKeys(in.Outputs) {
			o := in.Outputs[cmd]
			fmt.Fprintf(&sb, "$ %s\nexit=%d", cmd, o.ExitCode)
			if o.TimedOut {
				sb.WriteString(" (timed out)")
			}
			sb.WriteString("\n")
			if s := clip(strings.TrimSpace(o.Stdout), 3000); s != "" {
				fmt.Fprintf(&sb, "stdout:\n%s\n", s)
			}
			if s := clip(strings.TrimSpace(o.Stderr), 3000); s != "" {
				fmt.Fprintf(&sb, "stderr:\n%s\n", s)
			}
			sb.WriteString("\n")
		}
		d.AcceptanceOutput = strings.TrimSpace(sb.String())
	}
	if in.Capture != nil {
		d.BaseSHA = in.Capture.BaseSHA
		d.ChangedCount = len(in.Capture.ChangedFiles)
		d.Diff = in.Capture.Diff
		if maxDiff > 0 && len(d.Diff) > maxDiff {
			d.Diff = d.Diff[:maxDiff]
			d.DiffTruncated = true
		}
		d.Diff = strings.TrimRight(d.Diff, "\n")
	}
	if in.Result != nil && in.Result.Result != nil {
		d.FinalMessage = clip(strings.TrimSpace(in.Result.Result.Text()), 6000)
		if so := in.Result.Result.StructuredOutput; len(so) > 0 && strings.TrimSpace(string(so)) != "null" {
			var pretty bytes.Buffer
			if json.Indent(&pretty, so, "", "  ") == nil {
				d.StructuredOutput = clip(pretty.String(), 8000)
			} else {
				d.StructuredOutput = clip(string(so), 8000)
			}
			if d.FinalMessage == strings.TrimSpace(string(so)) {
				d.FinalMessage = "" // the CLI mirrors structured output into result.result
			}
		}
	}
	var sb strings.Builder
	if err := promptTmpl.Execute(&sb, d); err != nil {
		return "", fmt.Errorf("judge prompt: %w", err)
	}
	return sb.String(), nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

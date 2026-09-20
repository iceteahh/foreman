package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/100xteam-ai/foreman/internal/task"
)

// Report delivers a read-only task kind (code_review, report, triage): there is
// no diff to push, the deliverable is the run's structured output. It writes
// the output as JSON next to a rendered Markdown version and hands both to an
// optional Notifier (Slack, e-mail, an issue comment).
//
// Delivery is idempotent by construction: the file names are derived from the
// run id, so a retried delivery overwrites rather than duplicates (design §9).
type Report struct {
	// Dir is where reports are written (<data_root>/reports).
	Dir string
	// Notify publishes the rendered report; nil writes the files only.
	Notify Notifier
}

// Notifier publishes a finished report somewhere a human will see it.
type Notifier interface {
	// NotifyReport posts the report and returns an artifact id for the run.
	NotifyReport(ctx context.Context, t *task.Task, r *task.Run, title, markdown string) (string, error)
}

// Name implements Adapter.
func (Report) Name() string { return "report" }

// Deliver implements Adapter.
func (a Report) Deliver(ctx context.Context, in *Input) ([]string, error) {
	out := in.Run.Output
	if len(bytes.TrimSpace(out)) == 0 || string(bytes.TrimSpace(out)) == "null" {
		// A read-only kind whose acceptance sets a json_schema cannot reach
		// delivery without one (checks.SchemaValidation fails first), so this
		// is a kind configured without a schema: fall back to the final message.
		return a.deliverText(ctx, in)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		pretty.Write(out)
	}
	dir := a.dir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("deliver: report dir: %w", err)
	}
	jsonPath := filepath.Join(dir, in.Run.ID+".json")
	if err := os.WriteFile(jsonPath, append(pretty.Bytes(), '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("deliver: write report: %w", err)
	}
	md := Markdown(in, pretty.String())
	mdPath := filepath.Join(dir, in.Run.ID+".md")
	if err := os.WriteFile(mdPath, []byte(md), 0o600); err != nil {
		return nil, fmt.Errorf("deliver: write report: %w", err)
	}
	artifacts := []string{"report:" + jsonPath, "report:" + mdPath}
	if a.Notify != nil {
		id, err := a.Notify.NotifyReport(ctx, in.Task, in.Run, Title(in), md)
		if err != nil {
			// The report is on disk; losing the notification must not lose the run.
			return artifacts, fmt.Errorf("deliver: notify: %w", err)
		}
		if id != "" {
			artifacts = append(artifacts, id)
		}
	}
	return artifacts, nil
}

// deliverText handles a read-only kind with no json_schema: the worker's final
// message is the deliverable.
func (a Report) deliverText(ctx context.Context, in *Input) ([]string, error) {
	text := ""
	if in.Result != nil && in.Result.Result != nil {
		text = strings.TrimSpace(in.Result.Result.Text())
	}
	if text == "" {
		return nil, fmt.Errorf("deliver: run %s produced neither structured_output nor a final message", in.Run.ID)
	}
	dir := a.dir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("deliver: report dir: %w", err)
	}
	md := Markdown(in, "")
	mdPath := filepath.Join(dir, in.Run.ID+".md")
	if err := os.WriteFile(mdPath, []byte(md), 0o600); err != nil {
		return nil, fmt.Errorf("deliver: write report: %w", err)
	}
	artifacts := []string{"report:" + mdPath}
	if a.Notify != nil {
		id, err := a.Notify.NotifyReport(ctx, in.Task, in.Run, Title(in), md)
		if err != nil {
			return artifacts, fmt.Errorf("deliver: notify: %w", err)
		}
		if id != "" {
			artifacts = append(artifacts, id)
		}
	}
	return artifacts, nil
}

func (a Report) dir() string {
	if a.Dir != "" {
		return a.Dir
	}
	return "reports"
}

// Markdown renders a read-only run as a report document: the header a reader
// needs to trust it, the structured output, and the worker's own summary.
func Markdown(in *Input, structured string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", Subject(in, 120))
	fmt.Fprintf(&sb, "| | |\n|---|---|\n| Kind | `%s` |\n| Repo | `%s@%s` |\n| Task | `%s` |\n| Run | `%s` (attempt %d) |\n| Requested by | %s |\n",
		in.Task.Kind, in.Task.Workspace.Repo, in.Task.Workspace.Ref, in.Task.ID, in.Run.ID, in.Run.Attempt, in.Task.RequestedBy)
	if in.Result != nil {
		m := in.Result.Metrics()
		fmt.Fprintf(&sb, "| Turns | %d |\n| Cost | $%.4f |\n", m.Turns, m.CostUSD)
	}
	if structured != "" {
		fmt.Fprintf(&sb, "\n## Findings\n\n```json\n%s\n```\n", strings.TrimSpace(structured))
	}
	if in.Result != nil && in.Result.Result != nil {
		if txt := strings.TrimSpace(in.Result.Result.Text()); txt != "" {
			fmt.Fprintf(&sb, "\n## Summary\n\n%s\n", truncate(txt, 6000))
		}
	}
	sb.WriteString("\n## Deterministic checks\n\n")
	sb.WriteString(in.Checks.Markdown())
	sb.WriteString("\n---\n_Produced by the agent harness. Read-only task: no file was changed._\n")
	return sb.String()
}

package slack

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// ReportNotifier posts a finished read-only run (code_review, report, triage)
// into the review channel. It satisfies deliver.Notifier.
//
// Slack is where these deliverables are actually read, but the report is
// already on disk by the time this runs: a Slack outage must not lose a run,
// so the caller treats a failure here as a delivery warning, not a loss.
type ReportNotifier struct {
	Channel *Channel
	Logger  *slog.Logger
}

// NotifyReport implements deliver.Notifier.
func (n *ReportNotifier) NotifyReport(ctx context.Context, t *task.Task, r *task.Run, title, markdown string) (string, error) {
	if n.Channel == nil || n.Channel.Token == "" {
		return "", nil
	}
	header := fmt.Sprintf("*%s*  ·  `%s`  ·  `%s@%s`", escape(title), escape(string(t.Kind)),
		escape(t.Workspace.Repo), escape(t.Workspace.Ref))
	blocks := []any{
		map[string]any{"type": "section", "text": mrkdwn(header)},
		map[string]any{"type": "section", "text": mrkdwn("```" + escape(clip(markdown, 2800)) + "```")},
		map[string]any{"type": "context", "elements": []any{
			mrkdwn(fmt.Sprintf("run `%s` · task `%s` · attempt %d · $%.4f", r.ID, t.ID, r.Attempt, r.Metrics.CostUSD)),
		}},
	}
	resp, err := n.Channel.call(ctx, "chat.postMessage", map[string]any{
		"channel": n.Channel.ChannelID, "text": title, "blocks": blocks,
	})
	if err != nil {
		return "", err
	}
	return "slack:" + resp.Channel + "/" + resp.TS, nil
}

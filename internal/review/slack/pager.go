package slack

import (
	"context"
	"log/slog"
)

// Pager posts operational alerts to the review channel: dead letters, budget
// breaches and golden-suite regressions (design §8 "alerting", plan Step 18).
// It satisfies both budget.Pager and deadletter.Pager.
type Pager struct {
	Channel *Channel
	// Mention is prepended so the alert actually wakes someone ("<!here>").
	Mention string
	Logger  *slog.Logger
}

// Page implements budget.Pager and deadletter.Pager. Alerting must never block
// or fail the caller, so a delivery error is logged, not returned.
func (p *Pager) Page(ctx context.Context, subject, detail string) {
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	log.Error("PAGE "+subject, "detail", detail)
	if p.Channel == nil || p.Channel.Token == "" {
		return
	}
	text := subject
	if p.Mention != "" {
		text = p.Mention + " " + subject
	}
	blocks := []any{
		map[string]any{"type": "section", "text": mrkdwn("*" + escape(text) + "*")},
		map[string]any{"type": "section", "text": mrkdwn("```" + escape(clip(detail, 2800)) + "```")},
	}
	if _, err := p.Channel.call(ctx, "chat.postMessage", map[string]any{
		"channel": p.Channel.ChannelID, "text": text, "blocks": blocks,
	}); err != nil {
		log.Error("posting the page to Slack failed", "subject", subject, "err", err)
	}
}

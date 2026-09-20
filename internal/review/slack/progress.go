package slack

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/100xteam-ai/foreman/internal/obs"
)

// ProgressChannel publishes the live run timeline as one Slack message per run
// that is edited in place (design §8 "live progress"), with the detail in a
// thread reply so the channel stays readable. It implements obs.ProgressSink.
type ProgressChannel struct {
	// Channel carries the bot token and target channel; required.
	Channel *Channel
	// ThreadDetail posts each update as a threaded reply as well as editing
	// the parent message. Off by default: the parent message is the timeline.
	ThreadDetail bool

	mu    sync.Mutex
	posts map[string]Ref
}

// Ref is where a run's progress message lives.
type Ref struct {
	Channel string
	TS      string
}

// Update implements obs.ProgressSink: post on the first update, edit after.
func (p *ProgressChannel) Update(ctx context.Context, runID string, snap obs.Snapshot) error {
	if p.Channel == nil || p.Channel.Token == "" {
		return nil // progress to Slack is optional
	}
	p.mu.Lock()
	if p.posts == nil {
		p.posts = map[string]Ref{}
	}
	ref, posted := p.posts[runID]
	p.mu.Unlock()

	blocks := progressBlocks(snap)
	text := snap.Line()
	if !posted {
		resp, err := p.Channel.call(ctx, "chat.postMessage", map[string]any{
			"channel": p.Channel.ChannelID, "text": text, "blocks": blocks,
		})
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.posts[runID] = Ref{Channel: resp.Channel, TS: resp.TS}
		p.mu.Unlock()
		return nil
	}
	if _, err := p.Channel.call(ctx, "chat.update", map[string]any{
		"channel": ref.Channel, "ts": ref.TS, "text": text, "blocks": blocks,
	}); err != nil {
		return err
	}
	if p.ThreadDetail && snap.Done {
		if _, err := p.Channel.call(ctx, "chat.postMessage", map[string]any{
			"channel": ref.Channel, "thread_ts": ref.TS, "text": progressDetail(snap),
		}); err != nil {
			return err
		}
	}
	if snap.Done {
		p.mu.Lock()
		delete(p.posts, runID)
		p.mu.Unlock()
	}
	return nil
}

// progressBlocks renders the snapshot as Block Kit.
func progressBlocks(s obs.Snapshot) []any {
	header := fmt.Sprintf("*%s* `%s`", statusEmoji(s), escape(s.RunID))
	if s.Kind != "" {
		header += "  " + escape(s.Kind)
	}
	if s.Model != "" {
		header += "  " + escape(s.Model)
	}
	fields := []any{
		mrkdwn(fmt.Sprintf("*Turns*\n%d", s.Turns)),
		mrkdwn(fmt.Sprintf("*Tool calls*\n%d", s.ToolCalls)),
		mrkdwn(fmt.Sprintf("*Elapsed*\n%s", s.Elapsed.Round(1e9))),
		mrkdwn("*Cost*\n" + costText(s)),
	}
	blocks := []any{
		map[string]any{"type": "section", "text": mrkdwn(header)},
		map[string]any{"type": "section", "fields": fields},
	}
	if len(s.Tools) > 0 {
		blocks = append(blocks, map[string]any{"type": "context",
			"elements": []any{mrkdwn("Tools: " + escape(toolSummary(s.Tools)))}})
	}
	if len(s.Files) > 0 {
		blocks = append(blocks, map[string]any{"type": "section",
			"text": mrkdwn("*Files edited*\n" + escape(strings.Join(clip5(s.Files), "\n")))})
	}
	if len(s.Commands) > 0 {
		blocks = append(blocks, map[string]any{"type": "section",
			"text": mrkdwn("*Last command*\n```" + escape(s.Commands[len(s.Commands)-1]) + "```")})
	}
	if len(s.Denials) > 0 {
		blocks = append(blocks, map[string]any{"type": "context",
			"elements": []any{mrkdwn("⚠️ denied: " + escape(strings.Join(s.Denials, ", ")))}})
	}
	if s.RateLimited {
		blocks = append(blocks, map[string]any{"type": "context",
			"elements": []any{mrkdwn("⏳ rate limited; the orchestrator is backing off")}})
	}
	if s.Done && s.Text != "" {
		blocks = append(blocks, map[string]any{"type": "section",
			"text": mrkdwn("*Worker's final message*\n" + escape(clip(s.Text, 600)))})
	}
	return blocks
}

func progressDetail(s obs.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s finished: %s\n", s.RunID, s.Outcome)
	if len(s.Files) > 0 {
		fmt.Fprintf(&b, "Files: %s\n", strings.Join(s.Files, ", "))
	}
	for _, c := range s.Commands {
		fmt.Fprintf(&b, "$ %s\n", c)
	}
	return b.String()
}

func statusEmoji(s obs.Snapshot) string {
	if !s.Done {
		return "⏳ running"
	}
	switch s.Outcome {
	case "completed":
		return "✅ worker finished"
	case "budget_exhausted":
		return "💸 budget exhausted"
	case "max_turns":
		return "🔁 turns exhausted"
	case "api_error":
		return "🚫 api error"
	case "crash":
		return "💥 crashed"
	}
	return "⚠️ " + s.Outcome
}

func costText(s obs.Snapshot) string {
	if s.Done && s.CostUSD > 0 {
		return fmt.Sprintf("%.4f USD", s.CostUSD)
	}
	return fmt.Sprintf("~%.4f USD", s.EstCostUSD)
}

func toolSummary(tools map[string]int) string {
	type kv struct {
		name string
		n    int
	}
	list := make([]kv, 0, len(tools))
	for k, v := range tools {
		list = append(list, kv{k, v})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && (list[j].n > list[j-1].n || (list[j].n == list[j-1].n && list[j].name < list[j-1].name)); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s×%d", e.name, e.n))
	}
	return strings.Join(parts, ", ")
}

func clip5(ss []string) []string {
	if len(ss) <= 5 {
		return ss
	}
	return append(append([]string(nil), ss[:5]...), fmt.Sprintf("+%d more", len(ss)-5))
}

var _ obs.ProgressSink = (*ProgressChannel)(nil)

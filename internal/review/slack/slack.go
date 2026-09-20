// Package slack posts review requests as Block Kit messages with
// Approve / Reject / Close buttons and turns the interactive callbacks into
// review.Decisions (plan Step 12). It talks to the Web API with net/http only.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
)

// DefaultBaseURL is the Slack Web API.
const DefaultBaseURL = "https://slack.com/api"

// Action ids on the buttons.
const (
	ActionApprove = "harness_approve"
	ActionReject  = "harness_reject"
	ActionClose   = "harness_close"
	// RejectCallback is the modal's callback_id.
	RejectCallback = "harness_reject_modal"
)

// Channel posts to one Slack channel.
type Channel struct {
	// Token is the bot token (xoxb-…); it needs chat:write and, for the reject
	// modal, the app must handle interactivity.
	Token string
	// ChannelID is where requests are posted ("#agent-review" or a C… id).
	ChannelID string
	// Mention is prepended to escalations ("<!here>", "<@U123>", "<!subteam^S123>").
	Mention string
	// BaseURL defaults to DefaultBaseURL (tests point it at a fake).
	BaseURL string
	HTTP    *http.Client
	Logger  *slog.Logger
	// SLA is shown in the escalation text.
	SLA time.Duration
}

func (c *Channel) Name() string { return "slack" }

func (c *Channel) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Channel) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Channel) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// apiResponse is the common Web API envelope.
type apiResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// call posts JSON to method and decodes the envelope.
func (c *Channel) call(ctx context.Context, method string, body any) (*apiResponse, error) {
	if c.Token == "" {
		return nil, errors.New("slack: bot token is empty")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("slack %s: HTTP %d, non-JSON body", method, resp.StatusCode)
	}
	if !out.OK {
		return nil, fmt.Errorf("slack %s: %s", method, out.Error)
	}
	return &out, nil
}

// Post implements review.Channel.
func (c *Channel) Post(ctx context.Context, req *review.Request) (review.Ref, error) {
	if c.ChannelID == "" {
		return review.Ref{}, errors.New("slack: channel is empty")
	}
	msg := map[string]any{"channel": c.ChannelID, "text": fallbackText(req), "blocks": Blocks(req, true), "unfurl_links": false}
	resp, err := c.call(ctx, "chat.postMessage", msg)
	if err != nil {
		return review.Ref{}, err
	}
	c.log().Info("review posted to slack", "run_id", req.Run.ID, "channel", resp.Channel, "ts", resp.TS)
	return review.Ref{Channel: resp.Channel, ID: resp.TS}, nil
}

// Escalate implements review.Channel: a broadcast thread reply with the mention.
func (c *Channel) Escalate(ctx context.Context, req *review.Request, ref review.Ref) error {
	text := fmt.Sprintf("%s review of *%s* (`%s`) is still waiting", strings.TrimSpace(c.Mention), escape(req.Title()), req.Run.ID)
	if c.SLA > 0 {
		text += fmt.Sprintf(" after %s", c.SLA.Round(time.Minute))
	}
	text += "."
	_, err := c.call(ctx, "chat.postMessage", map[string]any{
		"channel": ref.Channel, "thread_ts": ref.ID, "reply_broadcast": true, "text": strings.TrimSpace(text),
	})
	return err
}

// Resolve implements review.Channel: the buttons are replaced by the decision.
func (c *Channel) Resolve(ctx context.Context, req *review.Request, ref review.Ref, d review.Decision, out *review.Outcome) error {
	blocks := Blocks(req, false)
	line := fmt.Sprintf(":white_check_mark: *%s* by %s at %s", strings.ToUpper(string(d.Action)[:1])+string(d.Action)[1:], escape(d.By), d.At.UTC().Format(time.RFC3339))
	if d.Action == review.ActionReject {
		line = strings.Replace(line, ":white_check_mark:", ":arrows_counterclockwise:", 1)
	}
	if d.Action == review.ActionClose {
		line = strings.Replace(line, ":white_check_mark:", ":no_entry_sign:", 1)
	}
	if d.Comment != "" {
		line += "\n> " + escape(clip(d.Comment, 500))
	}
	if out != nil && out.Message != "" {
		line += "\n" + escape(clip(out.Message, 500))
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []any{mrkdwn(line)}})
	_, err := c.call(ctx, "chat.update", map[string]any{"channel": ref.Channel, "ts": ref.ID, "text": fallbackText(req), "blocks": blocks})
	return err
}

func fallbackText(req *review.Request) string {
	return fmt.Sprintf("Review needed: %s (%s) — %s", req.Title(), req.Run.ID, req.Reason)
}

// Blocks renders the Block Kit body; withActions adds the buttons.
func Blocks(req *review.Request, withActions bool) []any {
	blocks := []any{
		map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": clip("Review: "+req.Title(), 150), "emoji": true}},
	}
	fields := []any{
		mrkdwn("*Task*\n`" + req.Task.ID + "` (" + string(req.Task.Kind) + ")"),
		mrkdwn(fmt.Sprintf("*Run*\n`%s` attempt %d", req.Run.ID, req.Run.Attempt)),
		mrkdwn("*Reason*\n" + escape(clip(req.Reason, 300))),
		mrkdwn("*Requested by*\n" + escape(req.Task.RequestedBy)),
	}
	if req.Phase != nil {
		fields = append(fields, mrkdwn(fmt.Sprintf("*Phase*\n%d of %d (%s)", req.Run.Phase, len(req.Task.Phases), req.Phase.Name)))
	}
	if m := req.Run.Metrics; m.CostUSD > 0 || m.Turns > 0 {
		fields = append(fields, mrkdwn(fmt.Sprintf("*Worker*\n%d turns · $%.3f · %ds", m.Turns, m.CostUSD, m.DurationMS/1000)))
	}
	blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	if s := req.Checks.Summary(); s != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": mrkdwn("*Checks*\n" + escape(s))})
	}
	if j := req.Judge; j != nil {
		var sb strings.Builder
		fmt.Fprintf(&sb, "*Judge*: %s", j.Verdict)
		if j.GamedChecks {
			sb.WriteString(" (gamed checks)")
		}
		if len(j.Scores) > 0 {
			keys := make([]string, 0, len(j.Scores))
			for k := range j.Scores {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var parts []string
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s %d", k, j.Scores[k]))
			}
			sb.WriteString(" · " + strings.Join(parts, ", "))
		}
		if r := strings.TrimSpace(j.Reasoning); r != "" {
			sb.WriteString("\n> " + strings.ReplaceAll(escape(clip(r, 1200)), "\n", "\n> "))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": mrkdwn(sb.String())})
	}
	if c := req.Capture; c != nil && len(c.ChangedFiles) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "*Diff*: %d file(s) vs `%s`\n```", len(c.ChangedFiles), short(c.BaseSHA))
		for i, st := range c.Stats {
			if i >= 20 {
				fmt.Fprintf(&sb, "\n… %d more", len(c.Stats)-20)
				break
			}
			fmt.Fprintf(&sb, "\n%s %s +%d/-%d", st.Status, st.Path, st.Added, st.Deleted)
		}
		sb.WriteString("\n```")
		blocks = append(blocks, map[string]any{"type": "section", "text": mrkdwn(sb.String())})
	}
	if len(req.Output) > 0 {
		var pretty bytes.Buffer
		txt := string(req.Output)
		if json.Indent(&pretty, req.Output, "", "  ") == nil {
			txt = pretty.String()
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": mrkdwn("*Output*\n```" + clip(txt, 2500) + "```")})
	}
	if withActions {
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "harness_review", "elements": []any{
			button(ActionApprove, "Approve", req.Run.ID, "primary"),
			button(ActionReject, "Reject…", req.Run.ID, ""),
			button(ActionClose, "Close", req.Run.ID, "danger"),
		}})
	}
	return blocks
}

func button(actionID, label, value, style string) map[string]any {
	b := map[string]any{"type": "button", "action_id": actionID, "text": map[string]any{"type": "plain_text", "text": label, "emoji": true}, "value": value}
	if style != "" {
		b["style"] = style
	}
	if actionID == ActionClose {
		b["confirm"] = map[string]any{
			"title":   map[string]any{"type": "plain_text", "text": "Close without delivering?"},
			"text":    mrkdwn("The run is closed and nothing is delivered or retried."),
			"confirm": map[string]any{"type": "plain_text", "text": "Close"}, "deny": map[string]any{"type": "plain_text", "text": "Cancel"},
		}
	}
	return b
}

func mrkdwn(s string) map[string]any { return map[string]any{"type": "mrkdwn", "text": s} }

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

var _ review.Channel = (*Channel)(nil)

package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/review"
)

// Handler serves the Slack interactivity endpoint (POST /slack/actions).
// Approve and Close apply immediately; Reject opens a modal for the comment
// and applies on view_submission. Every request is signature-checked.
type Handler struct {
	SigningSecret string
	Decider       review.Decider
	// Channel is used to open the reject modal (views.open); nil disables the
	// modal and rejects without a comment.
	Channel *Channel
	Logger  *slog.Logger
	// Now and MaxSkew make replay protection testable (default 5 minutes).
	Now     func() time.Time
	MaxSkew time.Duration
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// VerifySignature checks X-Slack-Signature (v0=hex(hmac-sha256(secret, "v0:" + ts + ":" + body)))
// and rejects timestamps outside maxSkew of now.
func VerifySignature(secret string, body []byte, timestamp, signature string, now time.Time, maxSkew time.Duration) error {
	if secret == "" {
		return errors.New("slack signing secret is empty")
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return errors.New("missing or invalid X-Slack-Request-Timestamp")
	}
	if maxSkew <= 0 {
		maxSkew = 5 * time.Minute
	}
	if d := now.Sub(time.Unix(ts, 0)); d > maxSkew || d < -maxSkew {
		return errors.New("request timestamp outside the allowed window")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%d:", ts)
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(strings.TrimSpace(signature))) {
		return errors.New("invalid X-Slack-Signature")
	}
	return nil
}

// Sign produces the headers Slack would send (tests and local tooling).
func Sign(secret string, body []byte, now time.Time) (timestamp, signature string) {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return ts, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// payload is the subset of Slack's interaction payload the handler reads.
type payload struct {
	Type      string `json:"type"`
	TriggerID string `json:"trigger_id"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	View struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		State           struct {
			Values map[string]map[string]struct {
				Value string `json:"value"`
			} `json:"values"`
		} `json:"state"`
	} `json:"view"`
}

func (p payload) by() string {
	switch {
	case p.User.Username != "":
		return "slack:" + p.User.Username
	case p.User.Name != "":
		return "slack:" + p.User.Name
	default:
		return "slack:" + p.User.ID
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := VerifySignature(h.SigningSecret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), h.now(), h.MaxSkew); err != nil {
		h.log().Warn("slack request rejected", "err", err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form body", http.StatusBadRequest)
		return
	}
	var p payload
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil {
		http.Error(w, "bad payload JSON", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	switch p.Type {
	case "block_actions":
		if len(p.Actions) == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		a := p.Actions[0]
		switch a.ActionID {
		case ActionApprove:
			h.decide(ctx, w, review.Decision{RunID: a.Value, Action: review.ActionApprove, By: p.by(), Source: "slack", At: h.now()})
		case ActionClose:
			h.decide(ctx, w, review.Decision{RunID: a.Value, Action: review.ActionClose, By: p.by(), Source: "slack", At: h.now()})
		case ActionReject:
			if h.Channel != nil && p.TriggerID != "" {
				if err := h.openRejectModal(ctx, p.TriggerID, a.Value); err == nil {
					w.WriteHeader(http.StatusOK)
					return
				}
				h.log().Warn("views.open failed; rejecting without comment", "err", err)
			}
			h.decide(ctx, w, review.Decision{RunID: a.Value, Action: review.ActionReject, By: p.by(), Source: "slack", At: h.now()})
		default:
			w.WriteHeader(http.StatusOK)
		}
	case "view_submission":
		if p.View.CallbackID != RejectCallback {
			w.WriteHeader(http.StatusOK)
			return
		}
		comment := ""
		for _, block := range p.View.State.Values {
			for _, v := range block {
				comment = v.Value
			}
		}
		d := review.Decision{RunID: p.View.PrivateMetadata, Action: review.ActionReject, By: p.by(), Comment: comment, Source: "slack", At: h.now()}
		if _, err := h.Decider.Decide(ctx, d); err != nil {
			// Surface the error inside the modal instead of leaving it hanging.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"response_action": "errors", "errors": map[string]string{"comment": err.Error()}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response_action": "clear"})
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (h *Handler) decide(ctx context.Context, w http.ResponseWriter, d review.Decision) {
	out, err := h.Decider.Decide(ctx, d)
	if err != nil {
		h.log().Warn("slack decision failed", "run_id", d.RunID, "action", d.Action, "err", err)
		// 200 with an ephemeral-style text keeps Slack from retrying the action.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response_type": "ephemeral", "text": "Could not apply " + string(d.Action) + ": " + err.Error()})
		return
	}
	h.log().Info("slack decision applied", "run_id", d.RunID, "action", d.Action, "by", d.By, "message", out.Message)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) openRejectModal(ctx context.Context, triggerID, runID string) error {
	view := map[string]any{
		"type": "modal", "callback_id": RejectCallback, "private_metadata": runID,
		"title":  map[string]any{"type": "plain_text", "text": "Reject and retry"},
		"submit": map[string]any{"type": "plain_text", "text": "Reject"},
		"close":  map[string]any{"type": "plain_text", "text": "Cancel"},
		"blocks": []any{
			map[string]any{"type": "input", "block_id": "comment", "optional": true,
				"label":   map[string]any{"type": "plain_text", "text": "What should the worker fix?"},
				"element": map[string]any{"type": "plain_text_input", "action_id": "comment", "multiline": true}},
			map[string]any{"type": "context", "elements": []any{mrkdwn("Run `" + runID + "` will be retried in the same session with your note as feedback.")}},
		},
	}
	_, err := h.Channel.call(ctx, "views.open", map[string]any{"trigger_id": triggerID, "view": view})
	return err
}

var _ http.Handler = (*Handler)(nil)

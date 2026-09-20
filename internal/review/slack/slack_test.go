package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/workspace"
)

// fakeSlack records Web API calls.
type fakeSlack struct {
	mu    sync.Mutex
	calls []struct {
		Method string
		Body   map[string]any
		Auth   string
	}
	fail string // method that returns ok:false
}

func (f *fakeSlack) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		method := strings.TrimPrefix(r.URL.Path, "/")
		f.mu.Lock()
		f.calls = append(f.calls, struct {
			Method string
			Body   map[string]any
			Auth   string
		}{method, body, r.Header.Get("Authorization")})
		f.mu.Unlock()
		if method == f.fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1700000000.000100"})
	})
}

func request() *review.Request {
	tk := &task.Task{ID: "tsk_01X", Kind: task.KindCodeFix, Title: "Fix <nil> crash & retry", Prompt: "p", RequestedBy: "webhook:github:o/r#4",
		Phases: []task.PhaseSpec{{Name: "plan", Review: true}, {Name: "implement"}}}
	r := &task.Run{ID: "run_01X", TaskID: tk.ID, Attempt: 2, Phase: 1, Status: task.StatusNeedsReview, Metrics: task.Metrics{Turns: 7, CostUSD: 0.1234, DurationMS: 65000}}
	return &review.Request{Task: tk, Run: r, Reason: "judge uncertain", Phase: tk.PhaseSpec(1),
		Checks:  checks.Report{Results: []checks.Result{{Name: "exit_code", Status: checks.Pass}, {Name: "diff_scope", Status: checks.NA}}},
		Judge:   &task.JudgeVerdict{Verdict: "uncertain", Scores: map[string]int{"task_completion": 6, "code_quality": 8}, Reasoning: "Cannot tell\nwithout tests."},
		Capture: &workspace.Capture{BaseSHA: "abcdef1234567890", ChangedFiles: []string{"a.go"}, Stats: []workspace.FileStat{{Path: "a.go", Added: 3, Deleted: 1, Status: "M"}}},
		Output:  json.RawMessage(`{"summary":"plan"}`)}
}

func TestChannelPostEscalateResolve(t *testing.T) {
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	ch := &Channel{Token: "xoxb-1", ChannelID: "#agent-review", Mention: "<!here>", BaseURL: srv.URL, SLA: 24 * time.Hour}
	req := request()
	ref, err := ch.Post(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Channel != "C123" || ref.ID != "1700000000.000100" || ref.String() != "C123/1700000000.000100" || review.ParseRef(ref.String()) != ref {
		t.Errorf("ref %+v", ref)
	}
	call := fs.calls[0]
	if call.Method != "chat.postMessage" || call.Auth != "Bearer xoxb-1" || call.Body["channel"] != "#agent-review" {
		t.Errorf("%+v", call)
	}
	blocks := blocksText(t, call.Body["blocks"])
	for _, want := range []string{"Review: Fix <nil> crash & retry", "`run_01X` attempt 2", "*Phase*\\n1 of 2 (plan)", "7 turns · $0.123 · 65s", "pass: exit_code · n/a: diff_scope",
		"*Judge*: uncertain · code_quality 8, task_completion 6", "> Cannot tell\\n> without tests.", "M a.go +3/-1", "abcdef12", `\"summary\": \"plan\"`,
		`"action_id":"harness_approve"`, `"action_id":"harness_reject"`, `"action_id":"harness_close"`, `"value":"run_01X"`, "Close without delivering?", "judge uncertain", "webhook:github:o/r#4"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("blocks missing %q in %s", want, blocks)
		}
	}
	// mrkdwn fields escape &, <, > (the plain_text header does not need to).
	escaped := blocksText(t, Blocks(&review.Request{Task: &task.Task{ID: "t", Title: "x"}, Run: &task.Run{ID: "r"}, Reason: "a <b> & c"}, false))
	if !strings.Contains(escaped, "*Reason*\\na &lt;b&gt; &amp; c") {
		t.Errorf("reason not escaped: %s", escaped)
	}

	if err := ch.Escalate(context.Background(), req, ref); err != nil {
		t.Fatal(err)
	}
	esc := fs.calls[1]
	if esc.Method != "chat.postMessage" || esc.Body["thread_ts"] != ref.ID || esc.Body["reply_broadcast"] != true || !strings.Contains(esc.Body["text"].(string), "<!here>") || !strings.Contains(esc.Body["text"].(string), "24h0m0s") {
		t.Errorf("%+v", esc)
	}

	d := review.Decision{RunID: "run_01X", Action: review.ActionReject, By: "slack:alice", Comment: "add tests", At: time.Unix(1700000000, 0)}
	if err := ch.Resolve(context.Background(), req, ref, d, &review.Outcome{Message: "retry queued as run_02"}); err != nil {
		t.Fatal(err)
	}
	upd := fs.calls[2]
	updated := blocksText(t, upd.Body["blocks"])
	if upd.Method != "chat.update" || upd.Body["ts"] != ref.ID || upd.Body["channel"] != "C123" || strings.Contains(updated, "harness_approve") ||
		!strings.Contains(updated, "*Reject* by slack:alice") || !strings.Contains(updated, "> add tests") || !strings.Contains(updated, "retry queued as run_02") {
		t.Errorf("%s", updated)
	}

	fs.fail = "chat.postMessage"
	if _, err := ch.Post(context.Background(), req); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("err %v", err)
	}
	if _, err := (&Channel{ChannelID: "x", BaseURL: srv.URL}).Post(context.Background(), req); err == nil {
		t.Error("empty token accepted")
	}
}

// blocksText renders blocks as JSON without HTML escaping so assertions read naturally.
func blocksText(t *testing.T, v any) string {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

type fakeDecider struct {
	mu        sync.Mutex
	decisions []review.Decision
	err       error
}

func (f *fakeDecider) Decide(_ context.Context, d review.Decision) (*review.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, d)
	if f.err != nil {
		return nil, f.err
	}
	return &review.Outcome{Message: "done"}, nil
}

func signedPost(t *testing.T, h http.Handler, secret string, payload any, now time.Time) *httptest.ResponseRecorder {
	t.Helper()
	pj, _ := json.Marshal(payload)
	body := []byte(url.Values{"payload": {string(pj)}}.Encode())
	ts, sig := Sign(secret, body, now)
	req := httptest.NewRequest(http.MethodPost, "/slack/actions", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestVerifySignature(t *testing.T) {
	body := []byte("payload=%7B%7D")
	now := time.Unix(1700000000, 0)
	ts, sig := Sign("s3cret", body, now)
	if err := VerifySignature("s3cret", body, ts, sig, now.Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"wrong secret": func() error { return VerifySignature("other", body, ts, sig, now, 0) },
		"tampered":     func() error { return VerifySignature("s3cret", []byte("payload=x"), ts, sig, now, 0) },
		"stale":        func() error { return VerifySignature("s3cret", body, ts, sig, now.Add(10*time.Minute), 0) },
		"future":       func() error { return VerifySignature("s3cret", body, ts, sig, now.Add(-10*time.Minute), 0) },
		"bad ts":       func() error { return VerifySignature("s3cret", body, "abc", sig, now, 0) },
		"empty secret": func() error { return VerifySignature("", body, ts, sig, now, 0) },
		"bad prefix": func() error {
			return VerifySignature("s3cret", body, ts, strings.Replace(sig, "v0=", "v1=", 1), now, 0)
		},
	} {
		if fn() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHandlerActions(t *testing.T) {
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	dec := &fakeDecider{}
	now := time.Unix(1700000000, 0)
	h := &Handler{SigningSecret: "s3cret", Decider: dec, Channel: &Channel{Token: "xoxb", BaseURL: srv.URL}, Now: func() time.Time { return now }}

	user := map[string]any{"id": "U1", "username": "alice"}
	// Approve applies immediately.
	rec := signedPost(t, h, "s3cret", map[string]any{"type": "block_actions", "user": user, "actions": []any{map[string]any{"action_id": ActionApprove, "value": "run_1"}}}, now)
	if rec.Code != 200 || len(dec.decisions) != 1 || dec.decisions[0].Action != review.ActionApprove || dec.decisions[0].RunID != "run_1" || dec.decisions[0].By != "slack:alice" || dec.decisions[0].Source != "slack" {
		t.Fatalf("%d %+v", rec.Code, dec.decisions)
	}
	// Close applies immediately.
	signedPost(t, h, "s3cret", map[string]any{"type": "block_actions", "user": user, "actions": []any{map[string]any{"action_id": ActionClose, "value": "run_2"}}}, now)
	if len(dec.decisions) != 2 || dec.decisions[1].Action != review.ActionClose {
		t.Fatalf("%+v", dec.decisions)
	}
	// Reject opens the modal (no decision yet).
	rec = signedPost(t, h, "s3cret", map[string]any{"type": "block_actions", "trigger_id": "trg", "user": user, "actions": []any{map[string]any{"action_id": ActionReject, "value": "run_3"}}}, now)
	if rec.Code != 200 || len(dec.decisions) != 2 || len(fs.calls) != 1 || fs.calls[0].Method != "views.open" || fs.calls[0].Body["trigger_id"] != "trg" {
		t.Fatalf("%d %+v %+v", rec.Code, dec.decisions, fs.calls)
	}
	view, _ := json.Marshal(fs.calls[0].Body["view"])
	if !strings.Contains(string(view), `"private_metadata":"run_3"`) || !strings.Contains(string(view), RejectCallback) {
		t.Errorf("%s", view)
	}
	// Modal submission applies the reject with the comment.
	rec = signedPost(t, h, "s3cret", map[string]any{"type": "view_submission", "user": user, "view": map[string]any{
		"callback_id": RejectCallback, "private_metadata": "run_3",
		"state": map[string]any{"values": map[string]any{"comment": map[string]any{"comment": map[string]any{"type": "plain_text_input", "value": "handle nil"}}}}}}, now)
	if rec.Code != 200 || len(dec.decisions) != 3 || dec.decisions[2].Action != review.ActionReject || dec.decisions[2].Comment != "handle nil" || dec.decisions[2].RunID != "run_3" {
		t.Fatalf("%d %+v", rec.Code, dec.decisions)
	}
	if !strings.Contains(rec.Body.String(), `"response_action":"clear"`) {
		t.Errorf("%s", rec.Body.String())
	}
	// Reject without a modal-capable channel applies directly.
	h2 := &Handler{SigningSecret: "s3cret", Decider: dec, Now: func() time.Time { return now }}
	signedPost(t, h2, "s3cret", map[string]any{"type": "block_actions", "user": map[string]any{"id": "U9"}, "actions": []any{map[string]any{"action_id": ActionReject, "value": "run_4"}}}, now)
	if len(dec.decisions) != 4 || dec.decisions[3].Action != review.ActionReject || dec.decisions[3].By != "slack:U9" {
		t.Fatalf("%+v", dec.decisions)
	}
	// Decider errors surface as an ephemeral message (200) or modal errors.
	dec.err = errors.New("run is closed")
	rec = signedPost(t, h, "s3cret", map[string]any{"type": "block_actions", "user": user, "actions": []any{map[string]any{"action_id": ActionApprove, "value": "run_5"}}}, now)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "run is closed") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	rec = signedPost(t, h, "s3cret", map[string]any{"type": "view_submission", "user": user, "view": map[string]any{"callback_id": RejectCallback, "private_metadata": "run_5"}}, now)
	if !strings.Contains(rec.Body.String(), `"response_action":"errors"`) {
		t.Errorf("%s", rec.Body.String())
	}
	// Bad signature and stale timestamp are rejected before anything is parsed.
	n := len(dec.decisions)
	if rec := signedPost(t, h, "wrong", map[string]any{"type": "block_actions"}, now); rec.Code != http.StatusUnauthorized {
		t.Errorf("code %d", rec.Code)
	}
	if rec := signedPost(t, h, "s3cret", map[string]any{"type": "block_actions"}, now.Add(-time.Hour)); rec.Code != http.StatusUnauthorized {
		t.Errorf("code %d", rec.Code)
	}
	if req := httptest.NewRequest(http.MethodGet, "/slack/actions", nil); func() int { r := httptest.NewRecorder(); h.ServeHTTP(r, req); return r.Code }() != http.StatusMethodNotAllowed {
		t.Error("GET accepted")
	}
	if len(dec.decisions) != n {
		t.Error("unsigned request reached the decider")
	}
	// Unknown interaction types are acknowledged and ignored.
	if rec := signedPost(t, h, "s3cret", map[string]any{"type": "shortcut"}, now); rec.Code != 200 || len(dec.decisions) != n {
		t.Errorf("%d", rec.Code)
	}
}

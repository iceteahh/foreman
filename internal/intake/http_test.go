package intake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/audit"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/config"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
	queuesqlite "github.com/100xteam-ai/harness-loop-platform-go/internal/queue/sqlite"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	storesqlite "github.com/100xteam-ai/harness-loop-platform-go/internal/store/sqlite"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/templates"
)

// fakeSubmit records specs and stores a task+run so GET endpoints work.
type fakeSubmit struct {
	store *storesqlite.Store
	q     *queuesqlite.Queue
	specs []task.Spec
}

func (f *fakeSubmit) Submit(ctx context.Context, spec task.Spec) (*task.Task, *task.Run, error) {
	f.specs = append(f.specs, spec)
	pol, err := templates.Policy(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	acc, _ := templates.Acceptance(spec.Kind)
	t, err := spec.Build(pol, acc, nil, nil, time.Now())
	if err != nil {
		return nil, nil, err
	}
	r := task.NewRun(t, time.Now())
	t.SessionID = r.SessionID
	if err := f.store.CreateTask(ctx, t); err != nil {
		return nil, nil, err
	}
	if err := f.store.CreateRun(ctx, r); err != nil {
		return nil, nil, err
	}
	_, err = f.q.Enqueue(ctx, queue.Job{RunID: r.ID, TaskID: t.ID, Kind: string(t.Kind)}, 0)
	return t, r, err
}

func newServer(t *testing.T, secret string) (*httptest.Server, *fakeSubmit, *audit.FileSink) {
	t.Helper()
	root := t.TempDir()
	st, err := storesqlite.Open(filepath.Join(root, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q, _ := queuesqlite.New(st.DB())
	aud, _ := audit.NewFileSink(filepath.Join(root, "audit"))
	fs := &fakeSubmit{store: st, q: q}
	s := &Server{Store: st, Queue: q, Audit: aud, Submit: fs, GitHub: GitHubOptions{Secret: secret, TriggerLabel: "harness", DefaultRef: "main"}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, fs, aud
}

func do(t *testing.T, method, url string, body any, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, url, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp, out
}

func TestPostTaskAndGet(t *testing.T) {
	srv, fs, aud := newServer(t, "")
	spec := map[string]any{"kind": "code_fix", "prompt": "Fix it", "workspace": map[string]any{"type": "git", "repo": "org/svc", "ref": "main"},
		"policy": map[string]any{"max_turns": 3}, "priority": 7}
	resp, body := do(t, "POST", srv.URL+"/tasks", spec, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d %v", resp.StatusCode, body)
	}
	tk := body["task"].(map[string]any)
	run := body["run"].(map[string]any)
	if tk["kind"] != "code_fix" || tk["policy"].(map[string]any)["max_turns"] != float64(3) || !strings.HasPrefix(tk["requested_by"].(string), "api:") {
		t.Errorf("task %v", tk)
	}
	if resp.Header.Get("Location") != "/runs/"+run["run_id"].(string) {
		t.Errorf("location %q", resp.Header.Get("Location"))
	}
	if len(fs.specs) != 1 || fs.specs[0].Priority != 7 {
		t.Errorf("specs %+v", fs.specs)
	}

	resp, got := do(t, "GET", srv.URL+"/runs/"+run["run_id"].(string), nil, nil)
	if resp.StatusCode != 200 || got["status"] != "queued" {
		t.Errorf("get run %d %v", resp.StatusCode, got)
	}
	resp, got = do(t, "GET", srv.URL+"/tasks/"+tk["task_id"].(string), nil, nil)
	if resp.StatusCode != 200 || got["prompt"] != "Fix it" {
		t.Errorf("get task %d %v", resp.StatusCode, got)
	}
	resp, _ = do(t, "GET", srv.URL+"/tasks/"+tk["task_id"].(string)+"/runs", nil, nil)
	if resp.StatusCode != 200 {
		t.Errorf("task runs %d", resp.StatusCode)
	}
	resp, _ = do(t, "GET", srv.URL+"/runs?status=queued", nil, nil)
	if resp.StatusCode != 200 {
		t.Errorf("list %d", resp.StatusCode)
	}
	resp, _ = do(t, "GET", srv.URL+"/runs?status=bogus", nil, nil)
	if resp.StatusCode != 400 {
		t.Errorf("bad status filter %d", resp.StatusCode)
	}
	resp, _ = do(t, "GET", srv.URL+"/runs/run_nope", nil, nil)
	if resp.StatusCode != 404 {
		t.Errorf("missing run %d", resp.StatusCode)
	}

	// Events: 404 until the audit log exists, then streamed as NDJSON.
	resp, _ = do(t, "GET", srv.URL+"/runs/"+run["run_id"].(string)+"/events", nil, nil)
	if resp.StatusCode != 404 {
		t.Errorf("events before log %d", resp.StatusCode)
	}
	w, _ := aud.Open(context.Background(), run["run_id"].(string))
	_ = w.Append([]byte(`{"type":"system"}`))
	_ = w.Append([]byte(`{"type":"result"}`))
	_ = w.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/runs/"+run["run_id"].(string)+"/events", nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	b, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != 200 || r2.Header.Get("Content-Type") != "application/x-ndjson" || strings.Count(string(b), "\n") != 2 {
		t.Errorf("events %d %q %q", r2.StatusCode, r2.Header.Get("Content-Type"), b)
	}

	resp, got = do(t, "GET", srv.URL+"/healthz", nil, nil)
	if resp.StatusCode != 200 || got["ok"] != true || got["queue"].(map[string]any)["ready"] != float64(1) {
		t.Errorf("healthz %d %v", resp.StatusCode, got)
	}
}

func TestPostTaskRejects(t *testing.T) {
	srv, _, _ := newServer(t, "")
	for name, body := range map[string]any{
		"unknown field": map[string]any{"kind": "code_fix", "prompt": "x", "bogus": 1, "workspace": map[string]any{"type": "git", "repo": "o/r", "ref": "main"}},
		"unknown kind":  map[string]any{"kind": "nope", "prompt": "x", "workspace": map[string]any{"type": "git", "repo": "o/r", "ref": "main"}},
		"no prompt":     map[string]any{"kind": "code_fix", "workspace": map[string]any{"type": "git", "repo": "o/r", "ref": "main"}},
		"not json":      "{{{",
	} {
		resp, _ := do(t, "POST", srv.URL+"/tasks", body, nil)
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func issuePayload(action, label string, labels ...string) []byte {
	ls := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	p := map[string]any{
		"action":     action,
		"issue":      map[string]any{"number": 42, "title": "Crash on empty input", "body": "Steps:\n1. run with no args", "html_url": "https://github.com/org/svc/issues/42", "labels": ls},
		"label":      map[string]string{"name": label},
		"repository": map[string]any{"full_name": "org/svc", "default_branch": "develop"},
		"sender":     map[string]string{"login": "alice"},
	}
	b, _ := json.Marshal(p)
	return b
}

func TestGitHubWebhook(t *testing.T) {
	const secret = "s3cret"
	srv, fs, _ := newServer(t, secret)
	url := srv.URL + "/webhooks/github"
	hdr := func(event string, body []byte) map[string]string {
		return map[string]string{"X-GitHub-Event": event, "X-GitHub-Delivery": "d-1", "X-Hub-Signature-256": sign(secret, body), "Content-Type": "application/json"}
	}

	// ping
	resp, body := do(t, "POST", url, []byte(`{"zen":"x"}`), hdr("ping", []byte(`{"zen":"x"}`)))
	if resp.StatusCode != 200 || body["pong"] != "d-1" {
		t.Errorf("ping %d %v", resp.StatusCode, body)
	}
	// bad signature
	p := issuePayload("labeled", "harness")
	h := hdr("issues", p)
	h["X-Hub-Signature-256"] = "sha256=deadbeef"
	if resp, _ := do(t, "POST", url, p, h); resp.StatusCode != 401 {
		t.Errorf("bad signature %d", resp.StatusCode)
	}
	// unrelated event
	if resp, _ := do(t, "POST", url, []byte(`{}`), hdr("push", []byte(`{}`))); resp.StatusCode != 202 {
		t.Errorf("push %d", resp.StatusCode)
	}
	// opened without the trigger label → ignored
	p = issuePayload("opened", "")
	if resp, _ := do(t, "POST", url, p, hdr("issues", p)); resp.StatusCode != 202 {
		t.Errorf("opened unlabelled %d", resp.StatusCode)
	}
	// labeled with a different label → ignored
	p = issuePayload("labeled", "bug")
	if resp, _ := do(t, "POST", url, p, hdr("issues", p)); resp.StatusCode != 202 {
		t.Errorf("wrong label %d", resp.StatusCode)
	}
	if len(fs.specs) != 0 {
		t.Fatalf("submitted %d tasks before a trigger", len(fs.specs))
	}
	// labeled with the trigger label → task
	p = issuePayload("labeled", "harness")
	resp, body = do(t, "POST", url, p, hdr("issues", p))
	if resp.StatusCode != 201 || body["run_id"] == nil {
		t.Fatalf("labeled %d %v", resp.StatusCode, body)
	}
	// opened with the label already present → task
	p = issuePayload("opened", "", "harness")
	if resp, _ := do(t, "POST", url, p, hdr("issues", p)); resp.StatusCode != 201 {
		t.Errorf("opened labelled %d", resp.StatusCode)
	}
	if len(fs.specs) != 2 {
		t.Fatalf("specs %d", len(fs.specs))
	}
	s := fs.specs[0]
	if s.Kind != task.KindCodeFix || s.Workspace.Repo != "org/svc" || s.Workspace.Ref != "develop" || s.RequestedBy != "webhook:github:org/svc#42" || s.Priority != 5 || s.Title != "Crash on empty input (#42)" {
		t.Errorf("spec %+v", s)
	}
	for _, want := range []string{"Issue #42: Crash on empty input", "run with no args", "issues/42", "`org/svc`", "`develop`"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, s.Prompt)
		}
	}
	// PR comment threads are ignored.
	var m map[string]any
	_ = json.Unmarshal(issuePayload("labeled", "harness"), &m)
	m["issue"].(map[string]any)["pull_request"] = map[string]any{}
	p, _ = json.Marshal(m)
	if resp, _ := do(t, "POST", url, p, hdr("issues", p)); resp.StatusCode != 202 {
		t.Errorf("pr thread %d", resp.StatusCode)
	}
}

func TestWebhookWithoutSecretSkipsVerification(t *testing.T) {
	srv, _, _ := newServer(t, "")
	p := issuePayload("labeled", "harness")
	resp, _ := do(t, "POST", srv.URL+"/webhooks/github", p, map[string]string{"X-GitHub-Event": "issues"})
	if resp.StatusCode != 201 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte("hello")
	if !VerifySignature("k", body, sign("k", body)) {
		t.Error("valid signature rejected")
	}
	if VerifySignature("k", body, sign("other", body)) || VerifySignature("k", body, "nope") {
		t.Error("invalid signature accepted")
	}
}

func TestCron(t *testing.T) {
	srv, fs, _ := newServer(t, "")
	_ = srv
	entries := []config.CronEntry{{Name: "nightly", Schedule: "@every 1h", Task: map[string]any{
		"kind": "code_fix", "prompt": "hi", "workspace": map[string]any{"type": "git", "repo": "o/r", "ref": "main"}}}}
	c, err := NewCron(entries, fs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Errorf("entries %d", c.Len())
	}
	c.fire("nightly", mustSpec(t, entries[0].Task))
	if len(fs.specs) != 1 || fs.specs[0].RequestedBy != "cron:nightly" {
		t.Errorf("specs %+v", fs.specs)
	}
	if _, err := NewCron([]config.CronEntry{{Name: "bad", Schedule: "not a schedule", Task: entries[0].Task}}, fs, nil); err == nil {
		t.Error("bad schedule accepted")
	}
	if _, err := NewCron([]config.CronEntry{{Name: "bad", Schedule: "@hourly", Task: map[string]any{"kind": "code_fix"}}}, fs, nil); err == nil {
		t.Error("task without prompt accepted")
	}
}

func mustSpec(t *testing.T, m map[string]any) task.Spec {
	s, err := SpecFromMap(m)
	if err != nil {
		t.Fatal(err)
	}
	s.RequestedBy = "cron:nightly"
	return s
}

type fakeDecider struct {
	last review.Decision
	err  error
}

func (f *fakeDecider) Decide(_ context.Context, d review.Decision) (*review.Outcome, error) {
	f.last = d
	if f.err != nil {
		return nil, f.err
	}
	return &review.Outcome{Message: "ok " + string(d.Action)}, nil
}

func TestReviewEndpoint(t *testing.T) {
	root := t.TempDir()
	st, err := storesqlite.Open(filepath.Join(root, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dec := &fakeDecider{}
	s := &Server{Store: st, Decider: dec, Reviews: st}
	h := s.Handler()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", "10.0.0.7")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := post("/runs/run_1/review", `{"action":"approve","by":"alice","comment":"ship it"}`)
	if rec.Code != 200 || dec.last.Action != review.ActionApprove || dec.last.RunID != "run_1" || dec.last.By != "alice" || dec.last.Source != "api" || dec.last.Comment != "ship it" {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body.String(), dec.last)
	}
	if rec := post("/runs/run_1/review", `{"action":"close"}`); rec.Code != 200 || dec.last.By != "10.0.0.7" {
		t.Errorf("%d by=%q", rec.Code, dec.last.By)
	}
	if rec := post("/runs/run_1/review", `{"action":"maybe","by":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad action %d", rec.Code)
	}
	if rec := post("/runs/run_1/review", `{"action":"approve","by":"x","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field %d", rec.Code)
	}
	dec.err = review.ErrNotReviewable
	if rec := post("/runs/run_1/review", `{"action":"approve","by":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("not reviewable %d", rec.Code)
	}
	dec.err = store.ErrNotFound
	if rec := post("/runs/run_1/review", `{"action":"approve","by":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("not found %d", rec.Code)
	}
	s.Decider = nil
	if rec := post("/runs/run_1/review", `{"action":"approve","by":"x"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("disabled %d", rec.Code)
	}

	// Decisions listing needs an existing run.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run_nope/decisions", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	// Extra handlers are mounted.
	s.Extra = map[string]http.Handler{"POST /slack/actions": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/slack/actions", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("extra handler not mounted: %d", rec.Code)
	}
}

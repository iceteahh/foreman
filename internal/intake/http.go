// Package intake normalises triggers (HTTP API, GitHub webhooks, cron) into
// task records and exposes read endpoints for runs.
package intake

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
	"strconv"
	"strings"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/audit"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/budget"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/templates"
)

// Submitter is implemented by orchestrator.Submitter.
type Submitter interface {
	Submit(ctx context.Context, spec task.Spec) (*task.Task, *task.Run, error)
}

// GitHubOptions configure the webhook handler.
type GitHubOptions struct {
	// Secret verifies X-Hub-Signature-256; empty disables verification (dev only).
	Secret string
	// TriggerLabel restricts issue triggers to issues carrying the label; "" = any opened issue.
	TriggerLabel string
	// DefaultRef is used when the payload lacks repository.default_branch.
	DefaultRef string
	// Priority for webhook-created tasks.
	Priority int
	// Kind is the task kind issues become (default code_fix).
	Kind task.Kind
}

// Server is the HTTP intake.
type Server struct {
	Store  store.Store
	Queue  queue.Queue
	Audit  audit.Sink
	Submit Submitter
	GitHub GitHubOptions
	// Budget refuses new work for a kind whose daily ceiling is reached
	// (429 + reason, design §8); nil disables the check.
	Budget *budget.Ledger
	// Decider applies review decisions (POST /runs/{id}/review); nil disables it.
	Decider review.Decider
	// Reviews lists decisions (GET /runs/{id}/decisions); nil disables it.
	Reviews review.Store
	// Extra handlers mounted verbatim ("POST /slack/actions").
	Extra   map[string]http.Handler
	Logger  *slog.Logger
	MaxBody int64
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Handler returns the routed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /tasks", s.postTask)
	mux.HandleFunc("GET /tasks/{id}", s.getTask)
	mux.HandleFunc("GET /tasks/{id}/runs", s.getTaskRuns)
	mux.HandleFunc("GET /tasks/{id}/children", s.getTaskChildren)
	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("GET /runs/{id}", s.getRun)
	mux.HandleFunc("GET /runs/{id}/events", s.getRunEvents)
	mux.HandleFunc("POST /webhooks/github", s.githubWebhook)
	mux.HandleFunc("POST /runs/{id}/review", s.postReview)
	mux.HandleFunc("GET /runs/{id}/decisions", s.getDecisions)
	mux.HandleFunc("GET /budget", s.getBudget)
	for pattern, h := range s.Extra {
		mux.Handle(pattern, h)
	}
	return s.limitBody(mux)
}

// reviewBody is the POST /runs/{id}/review payload.
type reviewBody struct {
	Action  review.Action `json:"action"`
	By      string        `json:"by"`
	Comment string        `json:"comment"`
}

func (s *Server) postReview(w http.ResponseWriter, r *http.Request) {
	if s.Decider == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("review decisions are not enabled"))
		return
	}
	var body reviewBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode review: %w", err))
		return
	}
	if body.By == "" {
		body.By = clientIP(r)
	}
	d := review.Decision{RunID: r.PathValue("id"), Action: body.Action, By: body.By, Comment: body.Comment, Source: "api", At: time.Now()}
	if err := d.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.Decider.Decide(r.Context(), d)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, review.ErrNotReviewable):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log().Info("review decision via api", "run_id", d.RunID, "action", d.Action, "by", d.By)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDecisions(w http.ResponseWriter, r *http.Request) {
	if s.Reviews == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("review store is not enabled"))
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.GetRun(r.Context(), id); s.notFound(w, err) {
		return
	}
	recs, err := s.Reviews.ListDecisions(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, map[string]any{"id": rec.ID, "run_id": rec.Decision.RunID, "task_id": rec.TaskID, "action": rec.Decision.Action,
			"by": rec.Decision.By, "comment": rec.Decision.Comment, "source": rec.Decision.Source, "at": rec.Decision.At,
			"next_run_id": rec.NextRunID, "judge_verdict": rec.JudgeVerdict, "checks_failed": rec.ChecksFailed})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	max := s.MaxBody
	if max <= 0 {
		max = 4 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true, "time": time.Now().UTC()}
	if s.Budget != nil {
		out["budget_breaker_open"] = s.Budget.BreakerOpen()
	}
	if s.Queue != nil {
		d, err := s.Queue.Depth(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		out["queue"] = map[string]any{"ready": d.Ready, "leased": d.Leased, "dead": d.Dead, "oldest_ready_seconds": int(d.OldestReadyAge.Seconds())}
	}
	writeJSON(w, http.StatusOK, out)
}

// budgetGate refuses a submission whose kind (or the global ceiling) is
// exhausted for the day, with 429 and the reason (design §8).
func (s *Server) budgetGate(w http.ResponseWriter, r *http.Request, kind task.Kind) bool {
	if s.Budget == nil {
		return true
	}
	err := s.Budget.Check(r.Context(), string(kind))
	if err == nil {
		return true
	}
	var ex *budget.ExhaustedError
	if errors.As(err, &ex) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(ex.ResetsAt)))
		s.log().Warn("task refused: daily budget exhausted", "kind", kind, "key", ex.Key, "spent_usd", ex.Spent, "limit_usd", ex.Limit)
	}
	writeErr(w, http.StatusTooManyRequests, err)
	return false
}

func retryAfterSeconds(resetsAt time.Time) int {
	d := int(time.Until(resetsAt).Seconds())
	if d < 1 {
		d = 1
	}
	return d
}

// getBudget reports today's spend against every ceiling (dashboards, doctor).
func (s *Server) getBudget(w http.ResponseWriter, r *http.Request) {
	if s.Budget == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("budgets are not configured"))
		return
	}
	rows, err := s.Budget.Report(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, sp := range rows {
		out = append(out, map[string]any{"key": sp.Key, "day": sp.Day, "spent_usd": sp.USD, "runs": sp.Runs,
			"limit_usd": sp.Limit, "remaining_usd": sp.Remaining(), "exceeded": sp.Exceeded()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"breaker_open": s.Budget.BreakerOpen(), "spend": out})
}

func (s *Server) postTask(w http.ResponseWriter, r *http.Request) {
	var spec task.Spec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode task spec: %w", err))
		return
	}
	if spec.RequestedBy == "" {
		spec.RequestedBy = "api:" + clientIP(r)
	}
	if !s.budgetGate(w, r, spec.Kind) {
		return
	}
	t, run, err := s.Submit.Submit(r.Context(), spec)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.log().Info("task submitted", "task_id", t.ID, "run_id", run.ID, "kind", t.Kind, "requested_by", t.RequestedBy)
	w.Header().Set("Location", "/runs/"+run.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"task": t, "run": run})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.GetTask(r.Context(), r.PathValue("id"))
	if s.notFound(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// getTaskChildren returns a fan-out parent's child tasks (plan Step 21).
func (s *Server) getTaskChildren(w http.ResponseWriter, r *http.Request) {
	kids, err := s.Store.ListChildTasks(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if kids == nil {
		kids = []*task.Task{}
	}
	writeJSON(w, http.StatusOK, kids)
}

func (s *Server) getTaskRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ListRunsByTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []*task.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	status := task.RunStatus(r.URL.Query().Get("status"))
	if status == "" {
		writeErr(w, http.StatusBadRequest, errors.New("status query parameter required"))
		return
	}
	if !status.Valid() {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown status %q", status))
		return
	}
	runs, err := s.Store.ListRunsByStatus(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []*task.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if s.notFound(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// getRunEvents streams the audit NDJSON. The run must exist; the log may not yet.
func (s *Server) getRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetRun(r.Context(), id); s.notFound(w, err) {
		return
	}
	rc, err := s.Audit.Reader(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no event log for run %s yet", id))
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) notFound(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
	return true
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

// --- GitHub webhook ---

type ghIssuePayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Labels  []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// VerifySignature checks X-Hub-Signature-256 against secret.
func VerifySignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, "sha256=")))
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if s.GitHub.Secret != "" && !VerifySignature(s.GitHub.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid webhook signature"))
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"pong": delivery})
		return
	case "issues":
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"ignored": "event " + event})
		return
	}
	var p ghIssuePayload
	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode issues payload: %w", err))
		return
	}
	if p.Issue.PullRequest != nil {
		writeJSON(w, http.StatusAccepted, map[string]string{"ignored": "pull request comment thread"})
		return
	}
	trigger := false
	switch p.Action {
	case "opened", "reopened":
		trigger = s.GitHub.TriggerLabel == "" || hasLabel(p, s.GitHub.TriggerLabel)
	case "labeled":
		trigger = s.GitHub.TriggerLabel != "" && p.Label.Name == s.GitHub.TriggerLabel
	}
	if !trigger {
		writeJSON(w, http.StatusAccepted, map[string]string{"ignored": fmt.Sprintf("issues.%s without trigger label %q", p.Action, s.GitHub.TriggerLabel)})
		return
	}
	ref := p.Repository.DefaultBranch
	if ref == "" {
		ref = s.GitHub.DefaultRef
	}
	if ref == "" {
		ref = "main"
	}
	kind := s.GitHub.Kind
	if kind == "" {
		kind = task.KindCodeFix
	}
	prompt, err := templates.Render(kind, map[string]any{
		"Repo": p.Repository.FullName, "Ref": ref, "Number": p.Issue.Number, "Title": p.Issue.Title,
		"Body": p.Issue.Body, "URL": p.Issue.HTMLURL,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	priority := s.GitHub.Priority
	if priority == 0 {
		priority = 5
	}
	spec := task.Spec{
		Kind:        kind,
		Title:       fmt.Sprintf("%s (#%d)", p.Issue.Title, p.Issue.Number),
		Prompt:      prompt,
		Workspace:   task.WorkspaceSpec{Type: "git", Repo: p.Repository.FullName, Ref: ref},
		Priority:    priority,
		RequestedBy: fmt.Sprintf("webhook:github:%s#%d", p.Repository.FullName, p.Issue.Number),
	}
	if !s.budgetGate(w, r, kind) {
		return
	}
	t, run, err := s.Submit.Submit(r.Context(), spec)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.log().Info("webhook task submitted", "task_id", t.ID, "run_id", run.ID, "issue", spec.RequestedBy, "delivery", delivery)
	w.Header().Set("Location", "/runs/"+run.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"task_id": t.ID, "run_id": run.ID})
}

func hasLabel(p ghIssuePayload, name string) bool {
	for _, l := range p.Issue.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

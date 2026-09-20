package deadletter

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

type memStore struct {
	mu      sync.Mutex
	entries map[string]Entry
	err     error
}

func newMem() *memStore { return &memStore{entries: map[string]Entry{}} }

func (m *memStore) SaveEntry(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.entries[e.RunID] = e
	return nil
}

func (m *memStore) GetEntry(_ context.Context, runID string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[runID]
	if !ok {
		return nil, ErrNotFound
	}
	return &e, nil
}

func (m *memStore) ListOpen(_ context.Context, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, e := range m.entries {
		if e.Open() && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memStore) MarkPaged(_ context.Context, runID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[runID]
	if !ok {
		return ErrNotFound
	}
	e.PagedAt = &at
	m.entries[runID] = e
	return nil
}

func (m *memStore) MarkRequeued(_ context.Context, runID, newRunID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[runID]
	if !ok {
		return ErrNotFound
	}
	e.RequeuedAt, e.RequeueRunID = &at, newRunID
	m.entries[runID] = e
	return nil
}

type fakePager struct {
	subjects []string
	details  []string
}

func (p *fakePager) Page(_ context.Context, subject, detail string) {
	p.subjects = append(p.subjects, subject)
	p.details = append(p.details, detail)
}

// deadRun is a run that failed its checks and was judged, as the router leaves it.
func deadRun() (*task.Task, *task.Run) {
	t := &task.Task{ID: task.NewTaskID(), Kind: task.KindCodeFix, Title: "Fix the parser"}
	r := &task.Run{
		ID: task.NewRunID(), TaskID: t.ID, Attempt: 3, Phase: 2,
		EventLogURI: "s3://harness-audit/runs/run_1.ndjson",
		LastError:   "checks failed: acceptance_commands · retries exhausted (attempt 3, max_retries 2)",
		Output:      json.RawMessage(`{"summary":"rewrote the tokenizer"}`),
		Eval: &task.Eval{
			Checks: map[string]task.CheckOutcome{
				"exit_code":           {Status: "pass"},
				"acceptance_commands": {Status: "fail", Evidence: "$ go test ./...\nexit=1\nFAIL parser: unexpected token"},
				"diff_scope":          {Status: "fail", Evidence: "out-of-scope files: vendor/x.go"},
			},
			Judge: &task.JudgeVerdict{Verdict: "fail", GamedChecks: true, Reasoning: "the test was deleted, not fixed"},
		},
	}
	return t, r
}

func TestEntryForCarriesTheFailureEvidence(t *testing.T) {
	tk, r := deadRun()
	e := EntryFor(tk, r, "retries exhausted")
	if e.RunID != r.ID || e.TaskID != tk.ID || e.Kind != "code_fix" || e.Attempt != 3 || e.Phase != 2 {
		t.Errorf("entry %+v", e)
	}
	if e.EventLogURI != r.EventLogURI || e.Reason != "retries exhausted" {
		t.Errorf("entry %+v", e)
	}
	if !e.Open() {
		t.Error("a fresh entry must be open")
	}
	for _, want := range []string{
		"failed checks: acceptance_commands, diff_scope",
		"FAIL parser: unexpected token",
		"out-of-scope files: vendor/x.go",
		"judge: fail (gamed_checks) — the test was deleted, not fixed",
		"last error: checks failed",
		"rewrote the tokenizer",
	} {
		if !strings.Contains(e.Feedback, want) {
			t.Errorf("feedback missing %q:\n%s", want, e.Feedback)
		}
	}
	// Passing checks are not listed as failures.
	if strings.Contains(e.Feedback, "exit_code:") {
		t.Errorf("a passing check was reported as evidence:\n%s", e.Feedback)
	}

	// A run with no eval at all still produces a usable entry.
	bare := &task.Run{ID: task.NewRunID(), TaskID: tk.ID, Attempt: 1, LastError: "provision workspace: no such repo"}
	be := EntryFor(tk, bare, "infrastructure failure")
	if !strings.Contains(be.Feedback, "provision workspace") || be.Phase != 0 {
		t.Errorf("bare entry %+v", be)
	}
}

func TestRecorderStoresPagesAndMarks(t *testing.T) {
	ctx := context.Background()
	st, pager := newMem(), &fakePager{}
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	rec := &Recorder{Store: st, Pager: pager, Logger: quiet, Now: func() time.Time { return at }}
	tk, r := deadRun()
	rec.Dead(ctx, EntryFor(tk, r, "retries exhausted"))

	got, err := st.GetEntry(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DeadAt.Equal(at) {
		t.Errorf("dead_at %s", got.DeadAt)
	}
	if got.PagedAt == nil || !got.PagedAt.Equal(at) {
		t.Errorf("paged_at %v", got.PagedAt)
	}
	if len(pager.subjects) != 1 {
		t.Fatalf("pages %v", pager.subjects)
	}
	if !strings.Contains(pager.subjects[0], r.ID) || !strings.Contains(pager.subjects[0], "code_fix") {
		t.Errorf("subject %q", pager.subjects[0])
	}
	for _, want := range []string{
		"exhausted its retries on attempt 3 of phase 2",
		"Reason: retries exhausted",
		"FAIL parser: unexpected token",
		"Event log: s3://harness-audit/runs/run_1.ndjson",
		"Replay: harness replay " + r.ID,
		"Requeue: harness requeue " + r.ID,
	} {
		if !strings.Contains(pager.details[0], want) {
			t.Errorf("page detail missing %q:\n%s", want, pager.details[0])
		}
	}

	// ListOpen sees it until it is requeued.
	if open, _ := st.ListOpen(ctx, 10); len(open) != 1 {
		t.Errorf("open %+v", open)
	}
	if err := st.MarkRequeued(ctx, r.ID, "run_next", at); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.ListOpen(ctx, 10); len(open) != 0 {
		t.Errorf("requeued entry still open: %+v", open)
	}

	// A custom replay hint (a log URL, say) replaces the default command.
	rec.ReplayHint = func(id string) string { return "https://logs.example/" + id }
	detail := rec.Detail(EntryFor(tk, r, "x"))
	if !strings.Contains(detail, "Replay: https://logs.example/"+r.ID) {
		t.Errorf("custom hint ignored:\n%s", detail)
	}
}

// Alerting must never take the caller down: a store failure is logged, and the
// page still goes out.
func TestRecorderSurvivesStoreFailureAndNoPager(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	st.err = ErrNotFound
	pager := &fakePager{}
	rec := &Recorder{Store: st, Pager: pager, Logger: quiet}
	tk, r := deadRun()
	rec.Dead(ctx, EntryFor(tk, r, "boom"))
	if len(pager.subjects) != 1 {
		t.Errorf("a store failure must not swallow the page: %v", pager.subjects)
	}

	// No pager, no store, nil recorder: all inert.
	(&Recorder{Logger: quiet}).Dead(ctx, Entry{RunID: r.ID})
	var nilRec *Recorder
	nilRec.Dead(ctx, Entry{RunID: r.ID})
}

func TestLogPagerWrites(t *testing.T) {
	var sb strings.Builder
	p := LogPager{Logger: slog.New(slog.NewTextHandler(&sb, nil))}
	p.Page(context.Background(), "subject here", "detail here")
	out := sb.String()
	if !strings.Contains(out, "PAGE subject here") || !strings.Contains(out, "detail here") {
		t.Errorf("log pager output %q", out)
	}
}

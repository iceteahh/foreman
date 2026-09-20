package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleTask() *task.Task {
	return &task.Task{
		ID: task.NewTaskID(), Kind: task.KindCodeFix, Prompt: "fix it",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"},
		Policy: task.Policy{AllowedTools: []string{"Read", "Edit"}, MaxTurns: 10, TimeoutMS: 60000, MaxCostUSD: 1, MaxRetries: 2,
			Judge: task.JudgePolicy{Enabled: true, Samples: 1, Threshold: 7}},
		Acceptance:  task.Acceptance{Commands: []string{"go test ./..."}, DiffScope: []string{"**"}},
		Priority:    5,
		RequestedBy: "test",
		CreatedAt:   time.Now().UTC().Truncate(time.Millisecond),
	}
}

func TestTaskAndRunRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := sampleTask()
	if err := s.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != tk.Prompt || got.Policy.MaxTurns != 10 || got.Acceptance.Commands[0] != "go test ./..." {
		t.Errorf("task mismatch: %+v", got)
	}
	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskSession(ctx, tk.ID, r.SessionID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, tk.ID)
	if got.SessionID != r.SessionID {
		t.Errorf("session pointer not updated: %q", got.SessionID)
	}
	gr, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gr.Status != task.StatusQueued || gr.SessionMode != task.SessionNew || gr.Artifacts == nil {
		t.Errorf("run mismatch: %+v", gr)
	}
	if n, _ := s.CountAttempts(ctx, tk.ID); n != 1 {
		t.Errorf("attempts = %d", n)
	}
}

func TestNotFound(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.GetTask(ctx, "tsk_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTask: %v", err)
	}
	if _, err := s.GetRun(ctx, "run_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetRun: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "run_nope", task.StatusRunning, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateRunStatus: %v", err)
	}
}

func TestCreateRejectsInvalid(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := sampleTask()
	tk.Prompt = ""
	if err := s.CreateTask(ctx, tk); err == nil {
		t.Error("invalid task accepted")
	}
	tk = sampleTask()
	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err == nil {
		t.Error("run for missing task accepted (foreign key)")
	}
}

func TestStatusTransitionsEnforced(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := sampleTask()
	if err := s.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	// Illegal edge is rejected at the store boundary and leaves the row untouched.
	err := s.UpdateRunStatus(ctx, r.ID, task.StatusPassed, "")
	var te *task.TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("queued -> passed should be a TransitionError, got %v", err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != task.StatusQueued {
		t.Fatalf("status changed after illegal transition: %s", got.Status)
	}

	for _, to := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusFailed} {
		if err := s.UpdateRunStatus(ctx, r.ID, to, "because"); err != nil {
			t.Fatalf("-> %s: %v", to, err)
		}
	}
	got, _ = s.GetRun(ctx, r.ID)
	if got.Status != task.StatusFailed || got.StartedAt == nil || got.FinishedAt == nil || got.LastError != "because" {
		t.Errorf("after failed: %+v", got)
	}
	// failed -> queued (retry) clears finished_at on the next running.
	if err := s.UpdateRunStatus(ctx, r.ID, task.StatusQueued, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, r.ID, task.StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetRun(ctx, r.ID)
	if got.FinishedAt != nil {
		t.Error("finished_at not cleared on re-run")
	}

	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM run_events WHERE run_id = ?`, r.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 { // created + 5 transitions
		t.Errorf("run_events rows = %d, want 6", n)
	}
}

func TestListAndRecord(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := sampleTask()
	if err := s.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	r1 := task.NewRun(tk, time.Now())
	r2 := task.NewRun(tk, time.Now().Add(time.Second))
	r2.Attempt = 2
	r2.SessionMode = task.SessionContinue
	r2.SessionID = r1.SessionID
	for _, r := range []*task.Run{r1, r2} {
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateRunStatus(ctx, r1.ID, task.StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	queued, err := s.ListRunsByStatus(ctx, task.StatusQueued)
	if err != nil || len(queued) != 1 || queued[0].ID != r2.ID {
		t.Fatalf("queued = %v, %v", queued, err)
	}
	byTask, _ := s.ListRunsByTask(ctx, tk.ID)
	if len(byTask) != 2 || byTask[0].Attempt != 1 || byTask[1].Attempt != 2 {
		t.Errorf("by task: %+v", byTask)
	}

	if err := s.RecordWorker(ctx, r1.ID, task.WorkerInfo{WorkspacePath: "/ws", Branch: "harness/x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMetrics(ctx, r1.ID, task.Metrics{Turns: 3, CostUSD: 0.5, TerminalReason: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEval(ctx, r1.ID, task.Eval{Checks: map[string]task.CheckOutcome{"exit_code": {Status: "pass"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtifacts(ctx, r1.ID, "file:///a.ndjson", "file:///s.jsonl", []string{"pr:org/svc#1", "pr:org/svc#1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, r1.ID)
	if got.Worker.Branch != "harness/x" || got.Metrics.Turns != 3 || got.Eval == nil || got.Eval.Checks["exit_code"].Status != "pass" ||
		got.EventLogURI != "file:///a.ndjson" || got.SessionURI != "file:///s.jsonl" || len(got.Artifacts) != 1 {
		t.Errorf("record mismatch: %+v", got)
	}
	if got.Status != task.StatusRunning {
		t.Error("record calls must not change status")
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s.Close()
}

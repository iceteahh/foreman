//go:build postgres

// Package postgres's tests need a real database and no tokens. Run them with
//
//	HARNESS_TEST_POSTGRES_DSN=postgres://harness:harness@localhost:5432/harness_test?sslmode=disable \
//	  go test -tags postgres ./internal/store/postgres/... ./internal/queue/postgres/...
//
// `make postgres-test` starts a throwaway container and does both.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/budget"
	"github.com/100xteam-ai/foreman/internal/deadletter"
	"github.com/100xteam-ai/foreman/internal/review"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
)

func open(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("HARNESS_TEST_POSTGRES_DSN is not set")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Each test starts from an empty schema: these tables are shared state and
	// a leftover run from a previous test would make LatestRun lie.
	for _, tbl := range []string{"run_events", "review_decisions", "review_posts", "dead_letters", "jobs", "runs", "tasks", "budget_spend", "leases"} {
		if _, err := s.pool.Exec(context.Background(), fmt.Sprintf("TRUNCATE TABLE %s CASCADE", tbl)); err != nil {
			// jobs only exists once the queue package has created it.
			t.Logf("truncate %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTask(t *testing.T, s *Store, parent string) *task.Task {
	t.Helper()
	tk := &task.Task{
		ID: task.NewTaskID(), Kind: task.KindCodeFix, Prompt: "fix it",
		Workspace:  task.WorkspaceSpec{Type: "git", Repo: "org/x", Ref: "main"},
		Policy:     task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 5, TimeoutMS: 1000, MaxCostUSD: 1, MaxRetries: 1},
		ParentID:   parent,
		CreatedAt:  time.Now().UTC(),
		SessionID:  task.NewSessionID(),
		Acceptance: task.Acceptance{Commands: []string{"go test ./..."}},
	}
	if err := s.CreateTask(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTaskAndRunRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := newTask(t, s, "")

	got, err := s.GetTask(ctx, tk.ID)
	if err != nil || got.Prompt != "fix it" || got.SessionID != tk.SessionID {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetTask(ctx, "tsk_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err %v", err)
	}

	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	// The state machine is enforced at the store boundary in both backends.
	if err := s.UpdateRunStatus(ctx, r.ID, task.StatusPassed, ""); err == nil {
		t.Error("queued → passed accepted")
	}
	for _, st := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusPassed, task.StatusDelivered} {
		if err := s.UpdateRunStatus(ctx, r.ID, st, "because"); err != nil {
			t.Fatalf("→ %s: %v", st, err)
		}
	}
	back, err := s.GetRun(ctx, r.ID)
	if err != nil || back.Status != task.StatusDelivered || back.LastError != "because" {
		t.Fatalf("%+v %v", back, err)
	}
	if back.StartedAt == nil || back.FinishedAt == nil {
		t.Errorf("timestamps: %+v", back)
	}

	if err := s.RecordMetrics(ctx, r.ID, task.Metrics{Turns: 3, CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutput(ctx, r.ID, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordArtifacts(ctx, r.ID, "s3://logs/x", "s3://sessions/y", []string{"branch:z", "branch:z"}); err != nil {
		t.Fatal(err)
	}
	back, _ = s.GetRun(ctx, r.ID)
	if back.Metrics.Turns != 3 || string(back.Output) != `{"a":1}` || len(back.Artifacts) != 1 {
		t.Errorf("%+v", back)
	}
	if n, err := s.CountAttempts(ctx, tk.ID); err != nil || n != 1 {
		t.Errorf("attempts %d %v", n, err)
	}
	latest, err := s.LatestRun(ctx, tk.ID)
	if err != nil || latest.ID != r.ID {
		t.Errorf("latest %v %v", latest, err)
	}
}

// A fan-out child links to its parent both ways in one transaction, so no
// replica ever sees a child whose parent does not list it (plan Step 21).
func TestFanOutLinksAndPhaseCAS(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	parent := newTask(t, s, "")
	// Give the parent phases so AdvancePhase has somewhere to go.
	if err := s.updateTask(ctx, parent.ID, func(tk *task.Task) error {
		tk.Phases = []task.PhaseSpec{
			{Name: "decompose", FanOut: true, Policy: tk.Policy},
			{Name: "synthesize", Policy: tk.Policy},
		}
		tk.Phase = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a := newTask(t, s, parent.ID)
	b := newTask(t, s, parent.ID)

	kids, err := s.ListChildTasks(ctx, parent.ID)
	if err != nil || len(kids) != 2 {
		t.Fatalf("children %d %v", len(kids), err)
	}
	stored, _ := s.GetTask(ctx, parent.ID)
	if len(stored.Children) != 2 {
		t.Errorf("parent children %v", stored.Children)
	}
	parents, err := s.ListFanOutParents(ctx)
	if err != nil || len(parents) != 1 || parents[0] != parent.ID {
		t.Errorf("parents %v %v", parents, err)
	}
	if a.ParentID != parent.ID || b.ParentID != parent.ID {
		t.Errorf("children do not name their parent: %q %q", a.ParentID, b.ParentID)
	}

	// Exactly one caller wins the fan-in, however many children finish at once.
	won, err := s.AdvancePhase(ctx, parent.ID, 1, 2)
	if err != nil || !won {
		t.Fatalf("first advance: won=%v err=%v", won, err)
	}
	won, err = s.AdvancePhase(ctx, parent.ID, 1, 2)
	if err != nil || won {
		t.Fatalf("second advance won the same phase: won=%v err=%v", won, err)
	}
	orphan := *a
	orphan.ID = task.NewTaskID()
	orphan.ParentID = "tsk_nope"
	if err := s.CreateTask(ctx, &orphan); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a child of an unknown parent was accepted: %v", err)
	}
}

func TestLeaseCompareAndSwap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })

	if ok, err := s.AcquireLease(ctx, "sweeper", "a", time.Minute); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := s.AcquireLease(ctx, "sweeper", "b", time.Minute); err != nil || ok {
		t.Fatalf("b took a held lease: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AcquireLease(ctx, "sweeper", "a", time.Minute); err != nil || !ok {
		t.Fatalf("holder could not renew: ok=%v err=%v", ok, err)
	}
	now = now.Add(2 * time.Minute)
	if ok, err := s.AcquireLease(ctx, "sweeper", "b", time.Minute); err != nil || !ok {
		t.Fatalf("expired lease not reclaimed: ok=%v err=%v", ok, err)
	}
	if err := s.ReleaseLease(ctx, "sweeper", "a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AcquireLease(ctx, "sweeper", "a", time.Minute); ok {
		t.Error("a released and retook a lease b owns")
	}
}

func TestReviewBudgetAndDeadLetter(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := newTask(t, s, "")
	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := s.SavePost(ctx, review.Post{RunID: r.ID, TaskID: tk.ID, Channel: "slack", Ref: review.ParseRef("C1/123.456"), PostedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	due, err := s.ListPostsForEscalation(ctx, now.Add(-24*time.Hour))
	if err != nil || len(due) != 1 {
		t.Fatalf("escalation candidates %d %v", len(due), err)
	}
	if err := s.MarkEscalated(ctx, r.ID, now); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.ListPostsForEscalation(ctx, now.Add(-24*time.Hour)); len(due) != 0 {
		t.Error("an escalated post is still a candidate")
	}
	if err := s.MarkResolved(ctx, "run_missing", now); !errors.Is(err, review.ErrPostNotFound) {
		t.Errorf("err %v", err)
	}
	id, err := s.RecordDecision(ctx, review.DecisionRecord{
		Decision: review.Decision{RunID: r.ID, Action: review.ActionApprove, By: "ops", At: now}, TaskID: tk.ID, JudgeVerdict: "uncertain", ChecksFailed: true})
	if err != nil || id == 0 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	recs, err := s.ListDecisions(ctx, r.ID)
	if err != nil || len(recs) != 1 || !recs[0].ChecksFailed || recs[0].JudgeVerdict != "uncertain" {
		t.Fatalf("%+v %v", recs, err)
	}
	if recs, _ := s.ListDecisionsSince(ctx, now.Add(-time.Hour), 10); len(recs) != 1 {
		t.Errorf("decisions since: %d", len(recs))
	}

	total, err := s.AddSpend(ctx, "code_fix", "2026-09-16", 1.25, 1)
	if err != nil || total != 1.25 {
		t.Fatalf("total %v err %v", total, err)
	}
	if total, _ = s.AddSpend(ctx, "code_fix", "2026-09-16", 0.75, 1); total != 2 {
		t.Errorf("total %v", total)
	}
	usd, runs, err := s.GetSpend(ctx, "code_fix", "2026-09-16")
	if err != nil || usd != 2 || runs != 2 {
		t.Errorf("%v %d %v", usd, runs, err)
	}
	if usd, _, _ := s.GetSpend(ctx, "code_fix", "2026-09-17"); usd != 0 {
		t.Errorf("an absent day is not zero: %v", usd)
	}
	rows, err := s.ListSpend(ctx, "2026-09-16")
	if err != nil || len(rows) != 1 || rows[0].Key != "code_fix" {
		t.Errorf("%+v %v", rows, err)
	}
	var _ budget.Store = s

	if err := s.SaveEntry(ctx, deadletter.Entry{RunID: r.ID, TaskID: tk.ID, Kind: "code_fix", Reason: "retries exhausted", Attempt: 3, DeadAt: now}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListOpen(ctx, 10)
	if err != nil || len(entries) != 1 || entries[0].Attempt != 3 {
		t.Fatalf("%+v %v", entries, err)
	}
	if err := s.MarkRequeued(ctx, r.ID, "run_next", now); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.ListOpen(ctx, 10); len(entries) != 0 {
		t.Error("a requeued entry is still open")
	}
	e, err := s.GetEntry(ctx, r.ID)
	if err != nil || e.RequeueRunID != "run_next" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := s.GetEntry(ctx, "run_missing"); !errors.Is(err, deadletter.ErrNotFound) {
		t.Errorf("err %v", err)
	}
}

func TestListTerminalTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := newTask(t, s, "")
	r := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.ListTerminalTasks(ctx, time.Now().Add(time.Hour)); len(ids) != 0 {
		t.Errorf("a queued run counted as terminal: %v", ids)
	}
	for _, st := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusPassed, task.StatusDelivered} {
		if err := s.UpdateRunStatus(ctx, r.ID, st, ""); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.ListTerminalTasks(ctx, time.Now().Add(time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != tk.ID {
		t.Fatalf("%v %v", ids, err)
	}
	if ids, _ := s.ListTerminalTasks(ctx, time.Now().Add(-time.Hour)); len(ids) != 0 {
		t.Errorf("swept a task that finished after the cutoff: %v", ids)
	}
}

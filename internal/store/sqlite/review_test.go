package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

func TestReviewStore(t *testing.T) {
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
	if _, err := s.GetPost(ctx, r.ID); !errors.Is(err, review.ErrPostNotFound) {
		t.Errorf("err %v", err)
	}
	posted := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	if err := s.SavePost(ctx, review.Post{RunID: r.ID, TaskID: tk.ID, Channel: "slack", Ref: review.Ref{Channel: "C1", ID: "1.2"}, PostedAt: posted}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPost(ctx, r.ID)
	if err != nil || p.Channel != "slack" || p.Ref.Channel != "C1" || p.Ref.ID != "1.2" || !p.PostedAt.Equal(posted) || p.EscalatedAt != nil {
		t.Fatalf("%+v %v", p, err)
	}
	due, _ := s.ListPostsForEscalation(ctx, time.Now().Add(-time.Hour))
	if len(due) != 1 {
		t.Fatalf("due %d", len(due))
	}
	if notDue, _ := s.ListPostsForEscalation(ctx, time.Now().Add(-3*time.Hour)); len(notDue) != 0 {
		t.Error("post inside SLA listed")
	}
	if err := s.MarkEscalated(ctx, r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.ListPostsForEscalation(ctx, time.Now()); len(due) != 0 {
		t.Error("escalated post listed again")
	}
	if err := s.MarkResolved(ctx, r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPost(ctx, r.ID); p.ResolvedAt == nil || p.EscalatedAt == nil {
		t.Errorf("%+v", p)
	}
	if err := s.MarkResolved(ctx, "run_nope", time.Now()); !errors.Is(err, review.ErrPostNotFound) {
		t.Errorf("err %v", err)
	}
	// Upsert replaces.
	if err := s.SavePost(ctx, review.Post{RunID: r.ID, TaskID: tk.ID, Channel: "log", Ref: review.Ref{Channel: "log", ID: r.ID}, PostedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPost(ctx, r.ID); p.Channel != "log" || p.ResolvedAt != nil {
		t.Errorf("%+v", p)
	}

	d := review.Decision{RunID: r.ID, Action: review.ActionReject, By: "alice", Comment: "again", Source: "slack", At: time.Now().UTC().Truncate(time.Millisecond)}
	id, err := s.RecordDecision(ctx, review.DecisionRecord{Decision: d, TaskID: tk.ID, NextRunID: "run_next", JudgeVerdict: "uncertain", ChecksFailed: false})
	if err != nil || id == 0 {
		t.Fatal(id, err)
	}
	if _, err := s.RecordDecision(ctx, review.DecisionRecord{Decision: review.Decision{RunID: r.ID, Action: "nope", By: "x"}}); err == nil {
		t.Error("bad action accepted")
	}
	recs, err := s.ListDecisions(ctx, r.ID)
	if err != nil || len(recs) != 1 {
		t.Fatal(recs, err)
	}
	got := recs[0]
	if got.Decision.Action != review.ActionReject || got.Decision.By != "alice" || got.Decision.Comment != "again" || got.NextRunID != "run_next" || got.JudgeVerdict != "uncertain" || !got.Decision.At.Equal(d.At) {
		t.Errorf("%+v", got)
	}

	// ListDecisionsSince is what `harness eval import-reviews` scans (Step 19).
	since, err := s.ListDecisionsSince(ctx, d.At.Add(-time.Hour), 0)
	if err != nil || len(since) != 1 || since[0].Decision.RunID != r.ID {
		t.Fatalf("%+v %v", since, err)
	}
	if after, err := s.ListDecisionsSince(ctx, d.At.Add(time.Hour), 0); err != nil || len(after) != 0 {
		t.Errorf("a decision older than the cutoff was returned: %+v %v", after, err)
	}
	if limited, err := s.ListDecisionsSince(ctx, d.At.Add(-time.Hour), 1); err != nil || len(limited) != 1 {
		t.Errorf("limit ignored: %+v %v", limited, err)
	}
}

func TestPhaseOutputLatestAndTerminalTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tk := sampleTask()
	tk.Kind = task.KindCodeFixPlanned
	tk.Phase = 1
	tk.Phases = []task.PhaseSpec{{Name: "plan", Review: true, Policy: tk.Policy, Acceptance: tk.Acceptance}, {Name: "impl", Policy: tk.Policy, Acceptance: tk.Acceptance, Prompt: "go"}}
	if err := s.CreateTask(ctx, tk); err != nil {
		t.Fatal(err)
	}
	r1 := task.NewRun(tk, time.Now())
	if err := s.CreateRun(ctx, r1); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutput(ctx, r1.ID, json.RawMessage(`{"plan":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOutput(ctx, r1.ID, json.RawMessage(`{bad`)); err == nil {
		t.Error("invalid output accepted")
	}
	got, _ := s.GetRun(ctx, r1.ID)
	if string(got.Output) != `{"plan":true}` {
		t.Errorf("output %s", got.Output)
	}
	if err := s.UpdateTaskPhase(ctx, tk.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskPhase(ctx, tk.ID, 3); err == nil {
		t.Error("phase 3 accepted")
	}
	gt, _ := s.GetTask(ctx, tk.ID)
	if gt.Phase != 2 {
		t.Errorf("phase %d", gt.Phase)
	}
	if err := s.UpdateTaskSession(ctx, tk.ID, r1.SessionID); err != nil {
		t.Fatal(err)
	}
	gt, _ = s.GetTask(ctx, tk.ID)
	r2, err := task.PhaseRun(gt, r1, 2, "go", time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if latest, _ := s.LatestRun(ctx, tk.ID); latest.ID != r2.ID {
		t.Errorf("latest %s want %s", latest.ID, r2.ID)
	}
	if _, err := s.LatestRun(ctx, "tsk_none"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err %v", err)
	}
	// Terminal only when the newest run is terminal and old enough.
	for _, st := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusPassed, task.StatusDelivered} {
		if err := s.UpdateRunStatus(ctx, r1.ID, st, ""); err != nil {
			t.Fatal(err)
		}
	}
	if ids, _ := s.ListTerminalTasks(ctx, time.Now().Add(time.Hour)); len(ids) != 0 {
		t.Errorf("task with a queued newest run listed: %v", ids)
	}
	for _, st := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusFailed, task.StatusDead} {
		if err := s.UpdateRunStatus(ctx, r2.ID, st, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if ids, _ := s.ListTerminalTasks(ctx, time.Now().Add(-time.Hour)); len(ids) != 0 {
		t.Errorf("recently finished task listed: %v", ids)
	}
	ids, err := s.ListTerminalTasks(ctx, time.Now().Add(time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != tk.ID {
		t.Errorf("%v %v", ids, err)
	}
}

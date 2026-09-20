package fanout

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/100xteam-ai/foreman/internal/task"
)

func child(status task.RunStatus) Child {
	return Child{Task: &task.Task{ID: "tsk_x", Title: "t"}, Run: &task.Run{ID: "run_x", Status: status}}
}

func TestDoneAndSettled(t *testing.T) {
	cases := []struct {
		status        task.RunStatus
		done, settled bool
	}{
		{task.StatusDelivered, true, true},
		{task.StatusDead, true, true},
		{task.StatusClosed, true, true},
		// The distinction the fan-in depends on: a child waiting for a human is
		// not finished, but the pool cannot move it either.
		{task.StatusNeedsReview, false, true},
		{task.StatusQueued, false, false},
		{task.StatusRunning, false, false},
		{task.StatusPassed, false, false},
	}
	for _, tc := range cases {
		c := child(tc.status)
		if c.Done() != tc.done || c.Settled() != tc.settled {
			t.Errorf("%s: done=%v settled=%v, want %v/%v", tc.status, c.Done(), c.Settled(), tc.done, tc.settled)
		}
	}
	// A child with no run at all has not started: it blocks the fan-in.
	none := Child{Task: &task.Task{ID: "tsk_y"}}
	if none.Done() || none.Settled() {
		t.Error("a child with no run counts as finished")
	}
	if AllDone(nil) || AllSettled(nil) {
		t.Error("a parent with no children must not count as fanned in")
	}
	mixed := []Child{child(task.StatusDelivered), child(task.StatusNeedsReview)}
	if AllDone(mixed) {
		t.Error("a child in review must not let the fan-in synthesise over it")
	}
	if !AllSettled(mixed) || len(Blocked(mixed)) != 1 {
		t.Errorf("settled=%v blocked=%d", AllSettled(mixed), len(Blocked(mixed)))
	}
	if Delivered(mixed) != 1 {
		t.Errorf("delivered %d", Delivered(mixed))
	}
}

func TestSummariesCarryEvidenceNotTranscripts(t *testing.T) {
	c := Child{
		Task: &task.Task{ID: "tsk_1", Title: "counter"},
		Run: &task.Run{ID: "run_1", Status: task.StatusDead, LastError: "retries exhausted",
			Artifacts: []string{"branch:harness/tsk_1@abc"},
			Output:    json.RawMessage(`{"k":1}`),
			Eval: &task.Eval{Checks: map[string]task.CheckOutcome{
				"tests": {Status: "fail", Evidence: "go test failed"}, "lint": {Status: "pass"},
			}}},
	}
	got := Summaries([]Child{c, {Task: &task.Task{ID: "tsk_2"}}})
	if len(got) != 2 {
		t.Fatalf("summaries %d", len(got))
	}
	if got[0].Status != "dead" || got[0].Error != "retries exhausted" {
		t.Errorf("%+v", got[0])
	}
	// Sorted so the synthesizer prompt is stable between runs.
	if got[0].Checks != "lint=pass tests=fail" {
		t.Errorf("checks %q", got[0].Checks)
	}
	if !strings.Contains(got[0].Output, `"k": 1`) {
		t.Errorf("output %q", got[0].Output)
	}
	// A child with no run still gets a row: a hole must be visible.
	if got[1].Title != "tsk_2" || got[1].Status != "not started" {
		t.Errorf("%+v", got[1])
	}
}

func TestCommandsFallBackToTheParentTask(t *testing.T) {
	parent := &task.Task{
		ID: "tsk_p", Prompt: "p",
		Acceptance: task.Acceptance{Commands: []string{"go test ./..."}},
		Child: &task.ChildSpec{Kind: task.KindCodeFix, SessionMode: task.SessionNew, MaxChildren: 4,
			Prompt: "{{ .Title }}"},
	}
	plan, err := Decode(json.RawMessage(`{"summary":"s","subtasks":[{"title":"a","detail":"d","files":["a.go"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	specs, _, err := plan.Specs(parent)
	if err != nil {
		t.Fatal(err)
	}
	var acc task.Acceptance
	if err := json.Unmarshal(specs[0].Acceptance, &acc); err != nil {
		t.Fatal(err)
	}
	// A child that runs no acceptance command is judged on a diff nobody compiled.
	if strings.Join(acc.Commands, ",") != "go test ./..." {
		t.Errorf("commands %v", acc.Commands)
	}
}

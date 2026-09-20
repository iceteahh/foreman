package review

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

func TestDecisionValidateAndRef(t *testing.T) {
	good := Decision{RunID: "run_1", Action: ActionApprove, By: "alice"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]Decision{
		"no run":     {Action: ActionApprove, By: "a"},
		"bad action": {RunID: "r", Action: "ship", By: "a"},
		"no by":      {RunID: "r", Action: ActionClose, By: " "},
	} {
		if d.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if !ActionReject.Valid() || Action("x").Valid() {
		t.Error("Action.Valid")
	}
	ref := Ref{Channel: "C1", ID: "1700.1"}
	if ParseRef(ref.String()) != ref || ParseRef("log").Channel != "log" {
		t.Errorf("%+v", ParseRef(ref.String()))
	}
}

func TestBuildAndLogChannel(t *testing.T) {
	tk := &task.Task{ID: "tsk_1", Kind: task.KindCodeFixPlanned, Prompt: "First line of the prompt\nsecond", Phases: []task.PhaseSpec{{Name: "plan", Review: true}, {Name: "impl"}}}
	r := &task.Run{ID: "run_1", TaskID: "tsk_1", Phase: 1, Status: task.StatusNeedsReview, LastError: "phase 1 (plan) requires approval",
		Output: []byte(`{"summary":"x"}`),
		Eval:   &task.Eval{Checks: map[string]task.CheckOutcome{"exit_code": {Status: "pass"}, "diff_scope": {Status: "fail", Evidence: "README.md"}}, Judge: &task.JudgeVerdict{Verdict: "uncertain", Reasoning: "hmm"}}}
	req := Build(tk, r, nil, "")
	if req.Reason != r.LastError || req.Phase == nil || req.Phase.Name != "plan" || string(req.Output) != `{"summary":"x"}` || req.Judge.Verdict != "uncertain" || !req.Checks.Failed() {
		t.Errorf("%+v", req)
	}
	if req.Title() != "First line of the prompt" {
		t.Errorf("title %q", req.Title())
	}
	tk.Title = "Given title"
	if Build(tk, r, nil, "explicit").Title() != "Given title" || Build(tk, r, nil, "explicit").Reason != "explicit" {
		t.Error("title/reason precedence")
	}

	var buf bytes.Buffer
	ch := LogChannel{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	ref, err := ch.Post(context.Background(), req)
	if err != nil || ref.Channel != "log" || ref.ID != "run_1" || ch.Name() != "log" {
		t.Fatalf("%+v %v", ref, err)
	}
	if err := ch.Escalate(context.Background(), req, ref); err != nil {
		t.Fatal(err)
	}
	if err := ch.Resolve(context.Background(), req, ref, Decision{RunID: "run_1", Action: ActionApprove, By: "bob"}, &Outcome{Message: "delivered"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"needs human review", "harness review -run run_1", "uncertain: hmm", "SLA expired", "decision applied", "by=bob", "delivered"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func basePolicy() Policy {
	return Policy{AllowedTools: []string{"Read", "Edit", "Write", "Bash(go test *)"}, MaxTurns: 30, TimeoutMS: 900000, MaxCostUSD: 2.5, MaxRetries: 2,
		Judge: JudgePolicy{Enabled: true, Samples: 1, Threshold: 7}, CriticalTools: []string{"Edit"}}
}

func TestMergeDoesNotAliasBase(t *testing.T) {
	base := basePolicy()
	want := strings.Join(base.AllowedTools, ",")
	over, err := base.Merge(json.RawMessage(`{"allowed_tools": ["Read", "Glob"], "judge": {"enabled": false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(over.AllowedTools, ",") != "Read,Glob" || over.Judge.Enabled || over.Judge.Samples != 1 || over.MaxTurns != 30 {
		t.Errorf("override %+v", over)
	}
	if got := strings.Join(base.AllowedTools, ","); got != want {
		t.Fatalf("base policy mutated by merge: %q", got)
	}
	over.AllowedTools[0] = "X"
	over.CriticalTools[0] = "Y"
	if base.AllowedTools[0] != "Read" || base.CriticalTools[0] != "Edit" {
		t.Error("merged policy shares memory with base")
	}
	acc := Acceptance{Commands: []string{"go test ./..."}, JSONSchema: json.RawMessage(`{"type":"object"}`), DiffScope: []string{"src/**"}}
	acc2, err := acc.Merge(json.RawMessage(`{"json_schema": {"type":"string"}, "expect_changes": false}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(acc.JSONSchema) != `{"type":"object"}` || acc2.ExpectChanges == nil || *acc2.ExpectChanges || acc2.Commands[0] != "go test ./..." {
		t.Errorf("acceptance merge: base=%s over=%+v", acc.JSONSchema, acc2)
	}
	if _, err := base.Merge(json.RawMessage(`{"max_turns": "x"}`)); err == nil {
		t.Error("bad override accepted")
	}
}

func TestBuildPhasesAndEffective(t *testing.T) {
	base := basePolicy()
	acc := Acceptance{Commands: []string{"go test ./..."}, DiffScope: []string{"**"}}
	phases, err := BuildPhases(base, acc, []PhaseTemplate{
		{Name: "plan", Review: true, Policy: json.RawMessage(`{"allowed_tools":["Read"],"judge":{"enabled":false,"samples":0,"threshold":0}}`),
			Acceptance: json.RawMessage(`{"commands":[],"expect_changes":false,"json_schema":{"type":"object"}}`)},
		{Name: "", Prompt: "Plan approved: {{ .Plan }}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 || phases[1].Name != "phase-2" || phases[0].Policy.AllowedTools[0] != "Read" || len(phases[0].Policy.AllowedTools) != 1 {
		t.Fatalf("%+v", phases)
	}
	if strings.Join(phases[1].Policy.AllowedTools, ",") != strings.Join(base.AllowedTools, ",") || phases[1].Acceptance.Commands[0] != "go test ./..." || phases[1].Acceptance.HasSchema() {
		t.Errorf("phase 2 must inherit the base unchanged: %+v", phases[1])
	}
	spec := Spec{Kind: KindCodeFixPlanned, Prompt: "fix", Workspace: WorkspaceSpec{Repo: "org/x", Ref: "main"}}
	tk, err := spec.Build(base, acc, []PhaseTemplate{
		{Name: "plan", Review: true, Policy: json.RawMessage(`{"allowed_tools":["Read"],"judge":{"enabled":false}}`), Acceptance: json.RawMessage(`{"expect_changes":false}`)},
		{Name: "implement", Prompt: "go"},
	}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tk.Phase != 1 || len(tk.Phases) != 2 || !tk.HasPhases() || tk.PhaseSpec(3) != nil || tk.PhaseSpec(0) != nil {
		t.Fatalf("%+v", tk)
	}
	r := NewRun(tk, time.Now())
	if r.Phase != 1 {
		t.Errorf("first run phase %d", r.Phase)
	}
	et := Effective(tk, r)
	if len(et.Policy.AllowedTools) != 1 || et.ExpectsChanges() || et.Prompt != "fix" || tk.Policy.AllowedTools[0] != "Read" || len(tk.Policy.AllowedTools) != 4 {
		t.Errorf("effective phase 1: %+v (task policy %+v)", et.Policy, tk.Policy)
	}
	tk.SessionID = NewSessionID()
	r2, err := PhaseRun(tk, r, 2, "go now", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r2.Phase != 2 || r2.Attempt != 1 || r2.SessionMode != SessionContinue || r2.SessionID != tk.SessionID || r2.RetryOf != r.ID || r2.Prompt != "go now" {
		t.Errorf("%+v", r2)
	}
	et2 := Effective(tk, r2)
	if !et2.ExpectsChanges() || et2.Prompt != "go now" || len(et2.Policy.AllowedTools) != 4 {
		t.Errorf("effective phase 2: %+v", et2)
	}
	if _, err := PhaseRun(tk, r, 3, "x", time.Now()); err == nil {
		t.Error("phase 3 accepted")
	}
	if err := r2.Validate(); err != nil {
		t.Error(err)
	}
	// A single-phase task with a phase set, or a phase out of range, is invalid.
	bad := *tk
	bad.Phase = 3
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("err %v", err)
	}
	single, _ := Spec{Kind: KindCodeFix, Prompt: "x", Workspace: WorkspaceSpec{Repo: "o/r", Ref: "main"}}.Build(base, acc, nil, nil, time.Now())
	if single.Phase != 0 || single.HasPhases() || NewRun(single, time.Now()).Phase != 0 {
		t.Errorf("%+v", single)
	}
	single.Phase = 1
	if err := single.Validate(); err == nil {
		t.Error("phase on single-phase task accepted")
	}
}

func TestNextRun(t *testing.T) {
	tk := &Task{ID: NewTaskID(), Kind: KindCodeFix, SessionID: NewSessionID()}
	prev := &Run{ID: NewRunID(), TaskID: tk.ID, Attempt: 1, SessionID: tk.SessionID, SessionMode: SessionNew}
	cont, err := NextRun(tk, prev, SessionContinue, "fix these", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cont.Attempt != 2 || cont.SessionID != tk.SessionID || cont.SessionMode != SessionContinue || cont.RetryOf != prev.ID || cont.Prompt != "fix these" || cont.Status != StatusQueued {
		t.Errorf("%+v", cont)
	}
	cold, err := NextRun(tk, prev, SessionNew, "orig + fb", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cold.SessionID == tk.SessionID || cold.SessionMode != SessionNew || cold.Attempt != 2 {
		t.Errorf("%+v", cold)
	}
	if err := cold.Validate(); err != nil {
		t.Error(err)
	}
	if _, err := NextRun(tk, prev, SessionFork, "", time.Now()); err == nil {
		t.Error("fork accepted")
	}
	if _, err := NextRun(&Task{ID: tk.ID}, prev, SessionContinue, "", time.Now()); err == nil {
		t.Error("continue without a session accepted")
	}
}

func TestRunOutputRoundTrip(t *testing.T) {
	r := &Run{ID: NewRunID(), TaskID: "tsk_x", Attempt: 1, Phase: 1, Prompt: "p", RetryOf: "run_y", Output: json.RawMessage(`{"a":1}`),
		SessionID: NewSessionID(), SessionMode: SessionNew, Status: StatusQueued}
	b, _ := json.Marshal(r)
	var back Run
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Phase != 1 || back.Prompt != "p" || back.RetryOf != "run_y" || string(back.Output) != `{"a":1}` {
		t.Errorf("%+v", back)
	}
	if !strings.Contains(string(b), `"phase":1`) || strings.Contains(string(b), `"output":null`) {
		t.Errorf("json %s", b)
	}
}

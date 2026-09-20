package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// designTask is the design §3.1 example document (with the Policy/Acceptance shape as declared).
const designTask = `{
  "task_id": "tsk_01J8000000000000000000TEST",
  "kind": "code_fix",
  "prompt": "…rendered from a template + trigger payload…",
  "workspace": { "type": "git", "repo": "org/service-a", "ref": "main", "write_access": false },
  "policy": {
    "allowed_tools": ["Read", "Edit", "Bash(git *)", "Bash(npm test)"],
    "max_turns": 30, "timeout_ms": 900000, "max_cost_usd": 2.50, "max_retries": 2,
    "judge": { "enabled": true, "samples": 1, "threshold": 7 }
  },
  "acceptance": { "commands": ["npm test", "npm run lint"], "json_schema": null, "diff_scope": ["src/**"], "custom_checks": [] },
  "priority": 5,
  "requested_by": "webhook:github:pr-4812",
  "created_at": "2026-09-13T04:12:00Z"
}`

const designRun = `{
  "run_id": "run_01J8000000000000000000TEST",
  "task_id": "tsk_01J8000000000000000000TEST",
  "attempt": 1,
  "session_id": "6f049bbf-9a65-44ed-9196-a589b89878fb",
  "session_mode": "new",
  "session_uri": "s3://harness-sessions/tsk_01J8/6f049bbf.jsonl",
  "status": "evaluating",
  "worker": { "container_id": "c1", "workspace_path": "/ws/run_01J8" },
  "metrics": { "turns": 14, "duration_ms": 412000, "cost_usd": 0.83, "exit_code": 0 },
  "eval": {
    "checks": { "tests": {"status":"pass"}, "lint": {"status":"pass"}, "schema": {"status":"n/a"},
                "exit_code": {"status":"pass"}, "permission_denials": {"status":"pass"}, "diff_scope": {"status":"pass"} },
    "judge": { "verdict": "pass", "scores": { "task_completion": 9, "minimal_diff": 8, "no_scope_creep": 9, "code_quality": 8 },
               "gamed_checks": false, "reasoning": "…" }
  },
  "event_log_uri": "s3://harness-audit/run_01J8.ndjson",
  "artifacts": ["pr:org/service-a#4813"],
  "created_at": "2026-09-13T04:12:00Z",
  "started_at": "2026-09-13T04:12:05Z",
  "finished_at": "2026-09-13T04:18:57Z"
}`

func roundTrip(t *testing.T, doc string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(doc), v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(doc), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	ac, _ := json.Marshal(a)
	bc, _ := json.Marshal(b)
	if string(ac) != string(bc) {
		t.Fatalf("round trip differs\n in: %s\nout: %s", ac, bc)
	}
}

func TestTaskRoundTripMatchesDesign(t *testing.T) {
	var tk Task
	roundTrip(t, designTask, &tk)
	if err := tk.Validate(); err != nil {
		t.Fatalf("design example should validate: %v", err)
	}
	if tk.Policy.Judge.Threshold != 7 || tk.Acceptance.DiffScope[0] != "src/**" {
		t.Errorf("fields not decoded: %+v", tk)
	}
	if tk.Acceptance.HasSchema() {
		t.Error("json_schema null should read as no schema")
	}
}

func TestRunRoundTripMatchesDesign(t *testing.T) {
	var r Run
	roundTrip(t, designRun, &r)
	if err := r.Validate(); err != nil {
		t.Fatalf("design example should validate: %v", err)
	}
	if r.Eval == nil || r.Eval.Judge.Scores["task_completion"] != 9 {
		t.Errorf("eval not decoded: %+v", r.Eval)
	}
}

func TestValidateCatchesEverything(t *testing.T) {
	var tk Task
	if err := json.Unmarshal([]byte(designTask), &tk); err != nil {
		t.Fatal(err)
	}
	tk.ID = "bad"
	tk.Kind = "nope"
	tk.Prompt = "  "
	tk.Workspace = WorkspaceSpec{Type: "svn"}
	tk.SessionID = "not-a-uuid"
	tk.Policy = Policy{AllowedTools: []string{"Read,Edit"}, MaxRetries: -1, Judge: JudgePolicy{Enabled: true, Threshold: 11}}
	tk.Acceptance.JSONSchema = json.RawMessage(`{not json`)
	err := tk.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"task_id", "kind", "prompt", "workspace.type", "workspace.repo", "workspace.ref",
		"session_id", "separator", "max_turns", "timeout_ms", "max_cost_usd", "max_retries", "judge.samples", "judge.threshold", "json_schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestPolicyMerge(t *testing.T) {
	base := Policy{AllowedTools: []string{"Read"}, MaxTurns: 30, TimeoutMS: 900000, MaxCostUSD: 2.5, MaxRetries: 2,
		Judge: JudgePolicy{Enabled: true, Samples: 1, Threshold: 7}}
	got, err := base.Merge(json.RawMessage(`{"max_turns": 5, "judge": {"samples": 3}, "allowed_tools": ["Read","Edit"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxTurns != 5 || got.TimeoutMS != 900000 || got.Judge.Samples != 3 || got.Judge.Threshold != 7 || len(got.AllowedTools) != 2 {
		t.Errorf("merge wrong: %+v", got)
	}
	if base.MaxTurns != 30 {
		t.Error("merge mutated base")
	}
	if same, _ := base.Merge(nil); same.MaxTurns != 30 {
		t.Error("nil override should be identity")
	}
	if _, err := base.Merge(json.RawMessage(`{"max_turns": "five"}`)); err == nil {
		t.Error("type mismatch should fail")
	}
}

func TestIDs(t *testing.T) {
	a, b := NewTaskID(), NewTaskID()
	if !strings.HasPrefix(a, "tsk_") || len(a) != 4+26 || a == b {
		t.Errorf("bad task ids %s %s", a, b)
	}
	if r := NewRunID(); !strings.HasPrefix(r, "run_") || len(r) != 4+26 {
		t.Errorf("bad run id %s", r)
	}
	s := NewSessionID()
	if u, err := uuid.Parse(s); err != nil || u.Version() != 4 {
		t.Errorf("session id %q is not a UUIDv4", s)
	}
	if NewSessionID() == s {
		t.Error("session ids must be unique")
	}
}

func TestNewRun(t *testing.T) {
	var tk Task
	if err := json.Unmarshal([]byte(designTask), &tk); err != nil {
		t.Fatal(err)
	}
	r := NewRun(&tk, time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Attempt != 1 || r.SessionMode != SessionNew || r.Status != StatusQueued || r.TaskID != tk.ID {
		t.Errorf("unexpected run %+v", r)
	}
}

func TestRunValidateForkRules(t *testing.T) {
	r := &Run{ID: "run_x", TaskID: "tsk_x", Attempt: 1, SessionID: "a", SessionMode: SessionFork, Status: StatusQueued}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "parent_session_id") {
		t.Errorf("fork without parent accepted: %v", err)
	}
	r.ParentSessionID = "a"
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "new session_id") {
		t.Errorf("fork reusing parent id accepted: %v", err)
	}
	r.ParentSessionID = "b"
	if err := r.Validate(); err != nil {
		t.Errorf("valid fork rejected: %v", err)
	}
	r.SessionMode = SessionContinue
	if err := r.Validate(); err == nil {
		t.Error("parent_session_id on a continue run accepted")
	}
}

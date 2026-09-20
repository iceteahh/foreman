package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/deliver"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/fanout"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// subtaskPlan is a planner result whose structured output decomposes the work
// into two file-disjoint subtasks.
const subtaskPlan = `{"type":"result","subtype":"success","is_error":false,"duration_ms":2500,"duration_api_ms":2400,"num_turns":3,` +
	`"result":"planned","session_id":"5b7d3f7e-3a1a-4f9e-9a5b-1e2d3c4b5a69","total_cost_usd":0.03,` +
	`"usage":{"input_tokens":10,"output_tokens":50},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"tool_use",` +
	`"structured_output":{"summary":"Split the rename across two files","subtasks":[` +
	`{"title":"Rename in a.go","detail":"Rename the helper in src/a.go and keep the exported name.","files":["src/a.go"]},` +
	`{"title":"Rename in b.go","detail":"Rename the helper in src/b.go to match.","files":["src/b.go"]}` +
	`],"risks":["the two files must agree"]},"uuid":"u"}`

const synthResult = `{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"duration_api_ms":1100,"num_turns":2,` +
	`"result":"merged","session_id":"5b7d3f7e-3a1a-4f9e-9a5b-1e2d3c4b5a69","total_cost_usd":0.01,` +
	`"usage":{"input_tokens":10,"output_tokens":20},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"end_turn",` +
	`"structured_output":{"title":"Rename done","summary":"Both files renamed","delivered":[],"gaps":[],"unknowns":[]},"uuid":"u"}`

func (e *env) fanoutSpec() task.Spec {
	return task.Spec{Kind: task.KindCodeFixFanout, Prompt: "Rename the helper everywhere",
		Workspace:  task.WorkspaceSpec{Type: "git", Repo: e.origin, Ref: "main"},
		Policy:     []byte(`{"max_turns": 5, "timeout_ms": 10000, "max_cost_usd": 1, "max_retries": 0}`),
		Acceptance: []byte(`{"commands": []}`), RequestedBy: "test"}
}

// fanoutClaude plays all three worker roles off one script: the planner emits
// the subtask list, each forked child edits the one file it owns, and the
// synthesizer emits the merge report. Children are told apart by the file
// their prompt names, which is exactly how a real worker would know.
func fanoutFakeBody() string {
	return `PROMPT=$(cat "$REC/argv.$N")
case "$PROMPT" in
  *"Merge their results"*) echo '` + synthResult + `' ;;
  *"src/a.go"*) printf 'package a\n\nfunc Renamed() {}\n' > src/a.go; cat "` + "REPLACE_FIXTURE" + `" ;;
  *"src/b.go"*) printf 'package a\n\nfunc RenamedToo() {}\n' > src/b.go; cat "` + "REPLACE_FIXTURE" + `" ;;
  *) echo '` + subtaskPlan + `' ;;
esac`
}

// driveTree processes until the whole fan-out tree rests: the parent's newest
// run is resting *and* no child is still running (what app.DriveTask does).
func (e *env) driveTree(t *testing.T, taskID string) *task.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		r := e.drive(t, taskID)
		pending, err := e.pool.FanOutPending(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			return r
		}
		if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func newFanoutEnv(t *testing.T) *env {
	t.Helper()
	body := strings.ReplaceAll(fanoutFakeBody(), "REPLACE_FIXTURE", fixture("result_success.json"))
	bin, _ := fakeClaude(t, body)
	return fanoutPool(t, newEnv(t, bin))
}

// fanoutPool gives the env what the real app wires for a fan-out kind: a
// submitter that can fork the planner transcript, and report delivery for the
// parent (whose synthesizer changes no file, so a branch push would fail).
func fanoutPool(t *testing.T, e *env) *env {
	t.Helper()
	e.pool.Submit = e.sub
	e.sub.Sessions = e.sess
	e.pool.Config.PerKind = map[string]int{}
	e.pool.Deliver = &deliver.ByKind{Default: deliver.BranchPush{}, Adapters: map[task.Kind]deliver.Adapter{
		task.KindCodeFixFanout: deliver.Report{Dir: filepath.Join(e.root, "reports")},
	}}
	return e
}

func TestFanOutRunsChildrenInParallelThenSynthesizes(t *testing.T) {
	e := newFanoutEnv(t)
	ctx := context.Background()
	parent, first, err := e.sub.Submit(ctx, e.fanoutSpec())
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionMode != task.SessionNew {
		t.Fatalf("planner session mode %q", first.SessionMode)
	}
	final := e.driveTree(t, parent.ID)
	if final.Status != task.StatusDelivered {
		t.Fatalf("parent run %s is %s (%s)", final.ID, final.Status, final.LastError)
	}

	// Two children, each a task of its own with the parent recorded both ways.
	kids, err := e.store.ListChildTasks(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("children: %d", len(kids))
	}
	stored, err := e.store.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Children) != 2 || stored.Children[0] != kids[0].ID {
		t.Errorf("parent does not name its children: %v", stored.Children)
	}
	for i, k := range kids {
		if k.ParentID != parent.ID {
			t.Errorf("child %d parent_id %q", i, k.ParentID)
		}
		if k.Kind != task.KindCodeFix {
			t.Errorf("child %d kind %q", i, k.Kind)
		}
		// The planner said who owns what; the child's diff scope must say the same.
		if len(k.Acceptance.DiffScope) != 1 {
			t.Errorf("child %d diff_scope %v", i, k.Acceptance.DiffScope)
		}
		runs := e.runs(t, k.ID)
		if runs[0].SessionMode != task.SessionFork {
			t.Errorf("child %d first run mode %q, want fork", i, runs[0].SessionMode)
		}
		if runs[0].ParentSessionID != first.SessionID {
			t.Errorf("child %d forks %q, want the planner session %q", i, runs[0].ParentSessionID, first.SessionID)
		}
		if runs[0].SessionID == first.SessionID {
			t.Errorf("child %d reuses the planner session id", i)
		}
		last := runs[len(runs)-1]
		if last.Status != task.StatusDelivered {
			t.Errorf("child %d run %s is %s (%s)", i, last.ID, last.Status, last.LastError)
		}
	}

	// The parent advanced to the synthesizer and delivered its report.
	parentRuns := e.runs(t, parent.ID)
	if len(parentRuns) != 2 {
		t.Fatalf("parent runs: %d", len(parentRuns))
	}
	synth := parentRuns[1]
	if synth.Phase != 2 || synth.SessionMode != task.SessionContinue {
		t.Errorf("synthesizer run %+v", synth)
	}
	if !strings.Contains(synth.Prompt, kids[0].ID) || !strings.Contains(synth.Prompt, "delivered") {
		t.Errorf("synthesizer prompt does not carry the children's outcomes:\n%s", synth.Prompt)
	}
	if synth.Status != task.StatusDelivered {
		t.Errorf("synthesizer is %s (%s)", synth.Status, synth.LastError)
	}
}

// A plan the schema accepts but the harness cannot decompose — here two
// subtasks claiming the same file, which would make two branches that clobber
// each other — is a human's problem, not a silent success.
func TestFanOutWithUndecomposablePlanGoesToReview(t *testing.T) {
	bad := strings.Replace(subtaskPlan, `"files":["src/b.go"]`, `"files":["src/a.go"]`, 1)
	bin, _ := fakeClaude(t, `echo '`+bad+`'`)
	e := fanoutPool(t, newEnv(t, bin))

	parent, _, err := e.sub.Submit(context.Background(), e.fanoutSpec())
	if err != nil {
		t.Fatal(err)
	}
	final := e.drive(t, parent.ID)
	if final.Status != task.StatusNeedsReview {
		t.Fatalf("run is %s, want needs_review (%s)", final.Status, final.LastError)
	}
	if !strings.Contains(final.LastError, "fan-out failed") {
		t.Errorf("reason %q", final.LastError)
	}
	if len(e.channel.posts) != 1 {
		t.Errorf("review posts: %d", len(e.channel.posts))
	}
	kids, _ := e.store.ListChildTasks(context.Background(), parent.ID)
	if len(kids) != 0 {
		t.Errorf("children created from an undecomposable plan: %d", len(kids))
	}
}

// The fan-in is a compare-and-swap: N children finishing at once must enqueue
// exactly one synthesizer.
func TestFanInEnqueuesOneSynthesizer(t *testing.T) {
	e := newFanoutEnv(t)
	ctx := context.Background()
	parent, _, err := e.sub.Submit(ctx, e.fanoutSpec())
	if err != nil {
		t.Fatal(err)
	}
	e.driveTree(t, parent.ID)

	// Every extra fan-in attempt, from a sibling or the sweeper, is a no-op.
	for i := 0; i < 3; i++ {
		if run, err := e.pool.maybeFanIn(ctx, parent.ID); err != nil {
			t.Fatal(err)
		} else if run != nil {
			t.Fatalf("attempt %d queued a second synthesizer %s", i, run.ID)
		}
	}
	sweeper := &FanInSweeper{Pool: e.pool, Logger: quiet}
	if n, err := sweeper.Once(ctx); err != nil || n != 0 {
		t.Fatalf("sweeper queued %d (err %v)", n, err)
	}
	if runs := e.runs(t, parent.ID); len(runs) != 2 {
		t.Fatalf("parent runs: %d, want planner + synthesizer", len(runs))
	}
}

// The sweeper is the crash recovery: a parent whose children all finished
// while nothing was watching still gets its synthesizer.
func TestFanInSweeperRecoversAMissedFanIn(t *testing.T) {
	e := newFanoutEnv(t)
	ctx := context.Background()
	parent, _, err := e.sub.Submit(ctx, e.fanoutSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Hide the parent link from the pool so the fast-path fan-in never fires:
	// that is what a harness looks like when it dies between the last child
	// finishing and the check.
	orig := e.pool.Store
	e.pool.Store = parentBlindStore{orig}
	for i := 0; i < 8; i++ {
		if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.pool.Store = orig
	if runs := e.runs(t, parent.ID); len(runs) != 1 {
		t.Fatalf("parent already advanced without the sweeper: %d runs", len(runs))
	}
	sweeper := &FanInSweeper{Pool: e.pool, Logger: quiet}
	n, err := sweeper.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sweeper queued %d synthesizers, want 1", n)
	}
	if runs := e.runs(t, parent.ID); len(runs) != 2 || runs[1].Phase != 2 {
		t.Fatalf("parent runs %+v", runs)
	}
}

// parentBlindStore hides a task's parent id, so a finished child never
// notifies its parent.
type parentBlindStore struct{ store.Store }

func (s parentBlindStore) GetTask(ctx context.Context, id string) (*task.Task, error) {
	t, err := s.Store.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	c := *t
	c.ParentID = ""
	return &c, nil
}

func TestFanOutPlanValidation(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"empty", `{"summary":"x","subtasks":[]}`, "no subtasks"},
		{"no files", `{"summary":"x","subtasks":[{"title":"a","detail":"d","files":[]}]}`, "names no files"},
		{"escapes the workspace", `{"summary":"x","subtasks":[{"title":"a","detail":"d","files":["../etc/passwd"]}]}`, "outside the workspace"},
		{"overlapping scopes", `{"summary":"x","subtasks":[{"title":"a","detail":"d","files":["src/a.go"]},{"title":"b","detail":"d","files":["src/a.go"]}]}`, "already claimed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fanout.Decode(json.RawMessage(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFanOutSpecsPinScopeAndCap(t *testing.T) {
	parent := &task.Task{
		ID: "tsk_parent", Kind: task.KindCodeFixFanout, Prompt: "do it", Priority: 7,
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "org/x", Ref: "main"},
		Child: &task.ChildSpec{Kind: task.KindCodeFix, SessionMode: task.SessionFork, MaxChildren: 2,
			Acceptance: json.RawMessage(`{"commands":["go test ./..."]}`),
			Prompt:     "{{ .Title }} | {{ .Detail }} | {{ range .Siblings }}{{ . }}{{ end }} | {{ .Index }}/{{ .Total }}"},
	}
	plan, err := fanout.Decode(json.RawMessage(`{"summary":"s","subtasks":[
		{"title":"one","detail":"d1","files":["src/b.go","src/a.go"]},
		{"title":"two","detail":"d2","files":["src/c.go"],"commands":["go vet ./..."]},
		{"title":"three","detail":"d3","files":["src/d.go"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	specs, dropped, err := plan.Specs(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || len(dropped) != 1 || dropped[0] != "three" {
		t.Fatalf("specs %d dropped %v", len(specs), dropped)
	}
	var acc task.Acceptance
	if err := json.Unmarshal(specs[0].Acceptance, &acc); err != nil {
		t.Fatal(err)
	}
	// Sorted, so the same plan always produces the same spec.
	if strings.Join(acc.DiffScope, ",") != "src/a.go,src/b.go" {
		t.Errorf("diff_scope %v", acc.DiffScope)
	}
	if strings.Join(acc.Commands, ",") != "go test ./..." {
		t.Errorf("commands %v", acc.Commands)
	}
	var acc2 task.Acceptance
	if err := json.Unmarshal(specs[1].Acceptance, &acc2); err != nil {
		t.Fatal(err)
	}
	if strings.Join(acc2.Commands, ",") != "go vet ./..." {
		t.Errorf("subtask commands ignored: %v", acc2.Commands)
	}
	if specs[0].ParentID != parent.ID || specs[0].Priority != 7 || specs[0].Workspace.Repo != "org/x" {
		t.Errorf("spec %+v", specs[0])
	}
	if specs[0].Prompt != "one | d1 | two | 1/2" {
		t.Errorf("child prompt %q", specs[0].Prompt)
	}
}

func TestSessionCopyMakesAForkResumable(t *testing.T) {
	e := newEnv(t, "/bin/true")
	ctx := context.Background()
	dir := t.TempDir()
	sid := task.NewSessionID()
	if err := writeTranscript(dir, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sess.Snapshot(ctx, "tsk_parent", sid, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sess.Copy(ctx, "tsk_parent", "tsk_child", sid); err != nil {
		t.Fatal(err)
	}
	restore := t.TempDir()
	if err := e.sess.Restore(ctx, "tsk_child", sid, restore, restore); err != nil {
		t.Fatalf("a forked child cannot restore the planner transcript: %v", err)
	}
	if _, err := e.sess.Copy(ctx, "tsk_missing", "tsk_child", sid); err == nil {
		t.Error("copying a snapshot that does not exist succeeded")
	}
}

// writeTranscript fakes what the CLI leaves under a CLAUDE_CONFIG_DIR.
func writeTranscript(configDir, sessionID string) error {
	dir := filepath.Join(configDir, "projects", "x")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o600)
}

// A fan-in that claims the phase but cannot queue the synthesizer must hand
// the claim back: otherwise the parent is stuck forever, with no pending
// children to trigger another fan-in and a phase the sweeper skips.
func TestFanInReleasesItsClaimWhenTheEnqueueFails(t *testing.T) {
	e := newFanoutEnv(t)
	ctx := context.Background()
	parent, _, err := e.sub.Submit(ctx, e.fanoutSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Drive only until the children are done, with the fan-in blinded.
	orig := e.pool.Store
	e.pool.Store = parentBlindStore{orig}
	for i := 0; i < 8; i++ {
		if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.pool.Store = orig

	e.pool.Queue = failingQueue{Queue: e.q}
	if _, err := e.pool.maybeFanIn(ctx, parent.ID); err == nil {
		t.Fatal("a failing enqueue was reported as success")
	}
	stored, err := e.store.GetTask(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != 1 {
		t.Fatalf("parent is stuck in phase %d with no synthesizer queued", stored.Phase)
	}
	// With the queue healthy again the sweeper finishes the job.
	e.pool.Queue = e.q
	sweeper := &FanInSweeper{Pool: e.pool, Logger: quiet}
	if n, err := sweeper.Once(ctx); err != nil || n != 1 {
		t.Fatalf("sweeper queued %d (err %v)", n, err)
	}
}

// failingQueue refuses new work but leaves everything else alone.
type failingQueue struct{ queue.Queue }

func (failingQueue) Enqueue(context.Context, queue.Job, time.Duration) (int64, error) {
	return 0, errors.New("queue is down")
}

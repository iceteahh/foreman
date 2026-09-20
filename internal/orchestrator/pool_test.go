package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/deliver"
	"github.com/100xteam-ai/foreman/internal/eval/judge"
	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/queue"
	queuesqlite "github.com/100xteam-ai/foreman/internal/queue/sqlite"
	"github.com/100xteam-ai/foreman/internal/review"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/session"
	storesqlite "github.com/100xteam-ai/foreman/internal/store/sqlite"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func fixture(name string) string {
	p, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "events", name))
	return p
}

// fakeClaude edits the workspace as instructed and replays a fixture. Like the
// CLI it writes a transcript under CLAUDE_CONFIG_DIR for --session-id runs and
// appends to the restored one for --resume runs. It records argv per
// invocation under $REC so tests can assert prompts. $MODE is "new" or "resume".
func fakeClaude(t *testing.T, body string) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	if err := os.MkdirAll(rec, 0o750); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "claude")
	script := `#!/bin/sh
REC="` + rec + `"
N=$(ls "$REC" | wc -l | tr -d ' '); N=$((N+1))
printf '%s\n' "$@" > "$REC/argv.$N"
SID=""; MODE="new"; prev=""
for a in "$@"; do
  [ "$prev" = "--session-id" ] && SID="$a"
  if [ "$prev" = "--resume" ]; then SID="$a"; MODE="resume"; fi
  prev="$a"
done
if [ -n "$SID" ]; then
  existing=$(ls "$CLAUDE_CONFIG_DIR"/projects/*/"$SID".jsonl 2>/dev/null | head -1)
  if [ "$MODE" = "resume" ] && [ -z "$existing" ]; then echo "No conversation found with session ID: $SID" >&2; exit 1; fi
  [ -z "$existing" ] && mkdir -p "$CLAUDE_CONFIG_DIR/projects/x" && existing="$CLAUDE_CONFIG_DIR/projects/x/$SID.jsonl"
  echo '{"type":"user"}' >> "$existing"
fi
# Like the real CLI (cli-contract #2), every event reports the session id the
# harness passed, whatever id the replayed fixture was captured with.
run_body() {
` + body + `
}
if sed -u p </dev/null >/dev/null 2>&1; then SEDU=-u; else SEDU=-l; fi   # line-buffered: GNU -u, BSD -l
run_body | sed $SEDU -e "s/\"session_id\":\"[0-9a-fA-F-]*\"/\"session_id\":\"$SID\"/g"
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

// argv returns the recorded argv of invocation n (1-based).
func argv(t *testing.T, rec string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rec, fmt.Sprintf("argv.%d", n)))
	if err != nil {
		t.Fatalf("invocation %d not recorded: %v", n, err)
	}
	return string(b)
}

// fakeJudge is a judge.Spawner returning a canned structured verdict.
type fakeJudge struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	tasks   []*task.Task
	runs    []*task.Run
	output  string // structured_output JSON; "" → crash
	fail    bool
}

func (f *fakeJudge) Run(_ context.Context, t *task.Task, r *task.Run, _ runner.Spawn) (*runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompts = append(f.prompts, t.Prompt)
	f.tasks = append(f.tasks, t)
	f.runs = append(f.runs, r)
	if f.fail {
		return &runner.Result{Outcome: events.OutcomeCrash, ExitCode: 137}, nil
	}
	return &runner.Result{Outcome: events.OutcomeCompleted, ExitCode: 0, Result: &events.ResultEvent{
		Subtype: "success", TerminalReason: "completed", SessionID: r.SessionID, NumTurns: 1, TotalCostUSD: 0.01,
		StructuredOutput: json.RawMessage(f.output)}}, nil
}

func verdictJSON(verdict string, min int, gamed bool) string {
	return fmt.Sprintf(`{"task_completion":%d,"minimal_diff":9,"no_scope_creep":9,"code_quality":8,"gamed_checks":%v,"verdict":%q,"reasoning":"canned"}`, min, gamed, verdict)
}

// fakeChannel records review posts.
type fakeChannel struct {
	mu        sync.Mutex
	posts     []*review.Request
	escalated []string
	resolved  []review.Decision
}

func (*fakeChannel) Name() string { return "fake" }
func (f *fakeChannel) Post(_ context.Context, req *review.Request) (review.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, req)
	return review.Ref{Channel: "C1", ID: fmt.Sprintf("%d", len(f.posts))}, nil
}
func (f *fakeChannel) Escalate(_ context.Context, req *review.Request, _ review.Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.escalated = append(f.escalated, req.Run.ID)
	return nil
}
func (f *fakeChannel) Resolve(_ context.Context, _ *review.Request, _ review.Ref, d review.Decision, _ *review.Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, d)
	return nil
}

type env struct {
	pool    *Pool
	sub     *Submitter
	store   *storesqlite.Store
	q       *queuesqlite.Queue
	sess    *session.Local
	origin  string
	root    string
	judge   *fakeJudge
	channel *fakeChannel
}

func newEnv(t *testing.T, claudeBin string) *env {
	t.Helper()
	root := t.TempDir()
	st, err := storesqlite.Open(filepath.Join(root, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q, err := queuesqlite.New(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := session.NewLocal(filepath.Join(root, "sessions"))
	aud, _ := audit.NewFileSink(filepath.Join(root, "audit"))
	wsm, _ := workspace.NewLocal(filepath.Join(root, "ws"))
	origin := workspace.NewBareRepo(t, map[string]string{
		"README.md": "# demo\n",
		"src/a.go":  "package a\n",
		"pass.sh":   "#!/bin/sh\nexit 0\n",
		"fail.sh":   "#!/bin/sh\necho tests failed >&2; exit 1\n",
		// check.sh passes once the worker has created src/fixed.
		"check.sh": "#!/bin/sh\n[ -f src/fixed ] && exit 0\necho 'TestThing failed: src/fixed missing' >&2; exit 1\n",
	})
	fj := &fakeJudge{output: verdictJSON("pass", 9, false)}
	ch := &fakeChannel{}
	pool := &Pool{
		Store: st, Queue: q, Workspaces: wsm, Deliver: deliver.BranchPush{}, Logger: quiet,
		Runner:  &runner.Runner{Bin: claudeBin, APIKey: "k", Sessions: sess, Audit: aud, Logger: quiet, Grace: 100 * time.Millisecond},
		Judge:   &judge.Judge{Spawner: fj, Logger: quiet},
		Review:  ch,
		Reviews: st,
		Config: PoolConfig{Global: 2, PerKind: map[string]int{"code_fix": 1}, PollInterval: 20 * time.Millisecond, Lease: 2 * time.Second,
			MaxJobAttempts: 2, APIErrorBackoff: 50 * time.Millisecond, ConfigDirRoot: filepath.Join(root, "cfg"), ShutdownGrace: 10 * time.Second},
	}
	return &env{pool: pool, sub: &Submitter{Store: st, Queue: q}, store: st, q: q, sess: sess, origin: origin, root: root, judge: fj, channel: ch}
}

// spec builds a code_fix spec with the judge disabled (M1 behaviour); tests
// that exercise Layer 2 override policy.
func (e *env) spec(cmds ...string) task.Spec {
	acc := `{"commands": [` + strings.Join(quoteAll(cmds), ",") + `], "diff_scope": ["src/**"]}`
	return task.Spec{Kind: task.KindCodeFix, Prompt: "Fix it", Workspace: task.WorkspaceSpec{Type: "git", Repo: e.origin, Ref: "main"},
		Policy:     []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 1, "judge": {"enabled": false}}`),
		Acceptance: []byte(acc), RequestedBy: "test"}
}

func (e *env) judgedSpec(cmds ...string) task.Spec {
	s := e.spec(cmds...)
	s.Policy = []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 1, "judge": {"enabled": true, "samples": 1, "threshold": 7}}`)
	return s
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = `"` + s + `"`
	}
	return out
}

func resting(s task.RunStatus) bool {
	switch s {
	case task.StatusDelivered, task.StatusDead, task.StatusFailed, task.StatusPassed, task.StatusNeedsReview, task.StatusClosed:
		return true
	}
	return false
}

// drive processes until the task's newest run rests and returns it.
func (e *env) drive(t *testing.T, taskID string) *task.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
			t.Fatalf("process: %v", err)
		}
		r, err := e.store.LatestRun(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if resting(r.Status) {
			return r
		}
		if d, _ := e.q.Depth(ctx); d.Ready == 0 && d.Leased == 0 && r.Status == task.StatusQueued {
			t.Fatalf("run queued but queue empty: %+v", r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) runs(t *testing.T, taskID string) []*task.Run {
	t.Helper()
	rs, err := e.store.ListRunsByTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestHappyPathDelivers(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n\nfunc Fixed() {}\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r, err := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if tk.SessionID != r.SessionID || tk.Policy.MaxTurns != 5 || tk.Policy.TimeoutMS != 5000 || len(tk.Policy.AllowedTools) == 0 || tk.HasPhases() {
		t.Errorf("submit merged wrong: %+v", tk)
	}
	var finished atomic.Int32
	e.pool.OnRunFinished = func(*task.Task, *task.Run) { finished.Add(1) }

	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDelivered {
		t.Fatalf("status %s last_error=%q eval=%+v", got.Status, got.LastError, got.Eval)
	}
	if len(got.Artifacts) != 1 || !strings.HasPrefix(got.Artifacts[0], "branch:harness/"+tk.ID+"@") {
		t.Errorf("artifacts %v", got.Artifacts)
	}
	if got.Metrics.CostUSD != 0.0196885 || got.Metrics.Turns != 1 || got.Eval == nil || got.Eval.Checks["acceptance_commands"].Status != "pass" || got.Eval.Judge != nil {
		t.Errorf("metrics/eval %+v %+v", got.Metrics, got.Eval)
	}
	if got.EventLogURI == "" || got.SessionURI == "" || got.Worker.Branch != "harness/"+tk.ID {
		t.Errorf("pointers %+v", got)
	}
	if !strings.Contains(got.LastError, "judge skipped") {
		t.Errorf("route reason %q", got.LastError)
	}
	// Session pointer follows result.session_id, which the CLI sets to the minted id.
	tk2, _ := e.store.GetTask(ctx, tk.ID)
	if tk2.SessionID != r.SessionID {
		t.Errorf("task session pointer %s want %s", tk2.SessionID, r.SessionID)
	}
	if out := workspace.Git(t, e.origin, "log", "--oneline", "harness/"+tk.ID); !strings.Contains(out, "harness: Fix it") {
		t.Errorf("origin branch:\n%s", out)
	}
	if entries, _ := os.ReadDir(e.pool.Config.ConfigDirRoot); len(entries) != 0 {
		t.Error("config dir not removed")
	}
	if _, err := os.Stat(got.Worker.WorkspacePath); !os.IsNotExist(err) {
		t.Error("workspace not destroyed")
	}
	if d, _ := e.q.Depth(ctx); d.Ready+d.Leased+d.Dead != 0 {
		t.Errorf("queue %+v", d)
	}
	if finished.Load() != 1 || e.judge.calls != 0 {
		t.Errorf("finished=%d judge calls=%d", finished.Load(), e.judge.calls)
	}
}

// Step 11 (a): run 1 fails a seeded test, run 2 resumes the session in a fresh
// workspace with the evidence as prompt and passes.
func TestRetryWithFeedbackResumesSession(t *testing.T) {
	bin, rec := fakeClaude(t, `printf 'package a\n// edit\n' > src/a.go
if [ "$MODE" = "resume" ]; then touch src/fixed; cat "`+fixture("result_resumed.json")+`"; else cat "`+fixture("result_success.json")+`"; fi`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r1, err := e.sub.Submit(ctx, e.spec("sh check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDelivered || got.ID == r1.ID || got.Attempt != 2 || got.SessionMode != task.SessionContinue || got.RetryOf != r1.ID {
		t.Fatalf("final run %+v", got)
	}
	// The retry resumed the session the first run ended with (result.session_id == minted id).
	if got.SessionID != r1.SessionID {
		t.Errorf("retry session %s want %s", got.SessionID, r1.SessionID)
	}
	first, _ := e.store.GetRun(ctx, r1.ID)
	if first.Status != task.StatusFailed || !strings.Contains(first.LastError, "acceptance_commands") || !contains(first.Artifacts, "retry:"+got.ID) {
		t.Errorf("first run %+v", first)
	}
	// Second invocation: --resume, prompt is the feedback with the verbatim check log.
	a2 := argv(t, rec, 2)
	if !strings.Contains(a2, "--resume\n"+got.SessionID) || strings.Contains(a2, "--session-id") {
		t.Errorf("retry argv:\n%s", a2)
	}
	for _, want := range []string{"Attempt 1 of this task was evaluated and rejected", "### acceptance_commands", "TestThing failed: src/fixed missing", "Do not start over"} {
		if !strings.Contains(a2, want) {
			t.Errorf("feedback prompt missing %q:\n%s", want, a2)
		}
	}
	if strings.Contains(a2, "\nFix it\n") {
		t.Error("a resumed retry must not repeat the original task prompt")
	}
	// Fresh workspace per attempt.
	if first.Worker.WorkspacePath == got.Worker.WorkspacePath {
		t.Error("attempts must not share a workspace")
	}
	// Retries exhausted → dead, and the failed run chain is recorded.
	if runs := e.runs(t, tk.ID); len(runs) != 2 {
		t.Errorf("runs %d", len(runs))
	}
}

// Step 11 (b): run 1 is SIGKILLed mid tool call; run 2 resumes the same session.
func TestCrashResumesSameSession(t *testing.T) {
	bin, rec := fakeClaude(t, `printf 'package a\n// edit\n' > src/a.go
if [ "$MODE" = "resume" ]; then cat "`+fixture("result_resumed.json")+`"; else cat "`+fixture("stream_sigkill_mid_tool_use.ndjson")+`"; kill -9 $$; fi`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r1, _ := e.sub.Submit(ctx, e.spec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDelivered || got.Attempt != 2 || got.SessionMode != task.SessionContinue {
		t.Fatalf("final %+v", got)
	}
	first, _ := e.store.GetRun(ctx, r1.ID)
	if first.Status != task.StatusFailed || first.Metrics.TerminalReason != "crash" || first.Metrics.ExitCode != 137 || first.SessionURI == "" {
		t.Errorf("first %+v", first)
	}
	// No result event → the minted id stays the resume pointer.
	if got.SessionID != r1.SessionID {
		t.Errorf("resume pointer %s want %s", got.SessionID, r1.SessionID)
	}
	if a2 := argv(t, rec, 2); !strings.Contains(a2, "--resume\n"+r1.SessionID) || !strings.Contains(a2, "exit_code") {
		t.Errorf("argv 2:\n%s", a2)
	}
}

// Step 11 (c): transcript deleted between runs → cold retry with a new id and
// the original prompt prepended.
func TestLostTranscriptColdRetries(t *testing.T) {
	bin, rec := fakeClaude(t, `printf 'package a\n// edit\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	spec := e.spec("sh fail.sh")
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 2, "judge": {"enabled": false}}`)
	tk, r1, _ := e.sub.Submit(ctx, spec)
	if ok, err := e.pool.ProcessOne(ctx, ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	first, _ := e.store.GetRun(ctx, r1.ID)
	if first.Status != task.StatusFailed || first.SessionURI == "" {
		t.Fatalf("first %+v", first)
	}
	// Delete the snapshot the retry would restore.
	if err := e.sess.Delete(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	// Attempt 2 (continue) cannot restore → session lost → attempt 3 is cold.
	got := e.drive(t, tk.ID)
	runs := e.runs(t, tk.ID)
	if len(runs) != 3 {
		t.Fatalf("runs %d", len(runs))
	}
	second := runs[1]
	if second.Status != task.StatusFailed || !strings.Contains(second.LastError, "session transcript lost") || second.SessionMode != task.SessionContinue || second.SessionID != r1.SessionID {
		t.Errorf("second %+v", second)
	}
	if got.SessionMode != task.SessionNew || got.SessionID == r1.SessionID || got.Attempt != 3 || got.RetryOf != second.ID {
		t.Errorf("third %+v", got)
	}
	// The CLI was spawned twice (attempt 2 never started); the cold prompt is
	// the original task plus the feedback under a fresh --session-id.
	a := argv(t, rec, 2)
	if _, err := os.Stat(filepath.Join(rec, "argv.3")); err == nil {
		t.Error("attempt 2 must not spawn the CLI when the transcript cannot be restored")
	}
	if !strings.Contains(a, "--session-id\n"+got.SessionID) || !strings.HasPrefix(a, "-p\nFix it\n") || !strings.Contains(a, "transcript is no longer available") || !strings.Contains(a, "no transcript snapshot for session") {
		t.Errorf("cold argv:\n%s", a)
	}
	// fail.sh still fails and attempt 3 > max_retries 2 → dead.
	if got.Status != task.StatusDead {
		t.Errorf("status %s %q", got.Status, got.LastError)
	}
}

func TestChecksFailRetriesThenDead(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// changed\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r, err := e.sub.Submit(ctx, e.spec("sh fail.sh"))
	if err != nil {
		t.Fatal(err)
	}
	got := e.drive(t, tk.ID) // max_retries 1 → attempt 2 dead
	if got.Status != task.StatusDead || got.Attempt != 2 || !strings.Contains(got.LastError, "retries exhausted") {
		t.Fatalf("status %s attempt %d last_error=%q", got.Status, got.Attempt, got.LastError)
	}
	first, _ := e.store.GetRun(ctx, r.ID)
	if first.Status != task.StatusFailed || !strings.Contains(first.LastError, "checks failed: acceptance_commands") {
		t.Errorf("first %+v", first)
	}
	if ev := got.Eval.Checks["acceptance_commands"]; ev.Status != "fail" || !strings.Contains(ev.Evidence, "tests failed") {
		t.Errorf("evidence %+v", ev)
	}
	if refs := workspace.Git(t, e.origin, "for-each-ref", "refs/heads/harness/"); strings.TrimSpace(refs) != "" {
		t.Error("failed run pushed a branch")
	}
	// max_retries 0 → dead on the first attempt.
	spec := e.spec("sh fail.sh")
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	tk2, _, _ := e.sub.Submit(ctx, spec)
	if got := e.drive(t, tk2.ID); got.Status != task.StatusDead || got.Attempt != 1 {
		t.Errorf("status %s attempt %d last_error=%q", got.Status, got.Attempt, got.LastError)
	}
}

func TestJudgePassDelivers(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDelivered || got.Eval.Judge == nil || got.Eval.Judge.Verdict != "pass" || got.Eval.Judge.Samples != 1 || got.Eval.Judge.CostUSD != 0.01 {
		t.Fatalf("%s %+v", got.Status, got.Eval)
	}
	if e.judge.calls != 1 {
		t.Fatalf("judge calls %d", e.judge.calls)
	}
	// The judge ran read-only, ephemeral, with the rubric schema, in the workspace, never seeing the transcript.
	jt, jr := e.judge.tasks[0], e.judge.runs[0]
	if jr.SessionMode != task.SessionEphemeral || jr.SessionID == got.SessionID || !strings.HasPrefix(jr.ID, got.ID+"-judge-") {
		t.Errorf("judge run %+v", jr)
	}
	if strings.Join(jt.Policy.AllowedTools, ",") != "Read" || jt.Policy.MaxTurns != 5 || jt.Policy.MaxCostUSD != 0.25 || !jt.Acceptance.HasSchema() {
		t.Errorf("judge policy %+v", jt.Policy)
	}
	p := e.judge.prompts[0]
	for _, want := range []string{"Fix it", "+// x", "pass: acceptance_commands", "sh pass.sh", "Reply only with the structured result"} {
		if !strings.Contains(p, want) {
			t.Errorf("judge prompt missing %q", want)
		}
	}
	if strings.Contains(p, "tool_use") || strings.Contains(p, `"type":"assistant"`) {
		t.Error("judge prompt leaks the transcript")
	}
	if !strings.Contains(got.LastError, "judge pass (min score 8)") {
		t.Errorf("reason %q", got.LastError)
	}
}

func TestJudgeFailRetriesWithReasoning(t *testing.T) {
	bin, rec := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	e.judge.output = verdictJSON("fail", 3, false)
	ctx := context.Background()
	tk, r1, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDead || got.Attempt != 2 || !strings.Contains(got.LastError, "judge: fail") {
		t.Fatalf("%s attempt %d %q", got.Status, got.Attempt, got.LastError)
	}
	first, _ := e.store.GetRun(ctx, r1.ID)
	if first.Status != task.StatusFailed || first.Eval.Judge.Verdict != "fail" {
		t.Errorf("first %+v", first.Eval)
	}
	if a2 := argv(t, rec, 2); !strings.Contains(a2, "Independent review (verdict: fail)") || !strings.Contains(a2, "canned") || !strings.Contains(a2, "task_completion=3") {
		t.Errorf("feedback lacks judge reasoning:\n%s", a2)
	}
	// Gamed checks is an automatic fail even with a pass verdict.
	e.judge.output = verdictJSON("pass", 9, true)
	tk2, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	if got := e.drive(t, tk2.ID); got.Status != task.StatusDead || !strings.Contains(got.LastError, "gamed") {
		t.Errorf("%s %q", got.Status, got.LastError)
	}
}

func TestJudgeUncertainOrLowScoreNeedsReview(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	ctx := context.Background()

	e.judge.output = verdictJSON("pass", 5, false) // min score 5 < threshold 7
	tk, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusNeedsReview || !strings.Contains(got.LastError, "below threshold 7") {
		t.Fatalf("%s %q", got.Status, got.LastError)
	}
	if len(e.channel.posts) != 1 || e.channel.posts[0].Run.ID != got.ID || e.channel.posts[0].Judge == nil || e.channel.posts[0].Capture == nil {
		t.Errorf("review post %+v", e.channel.posts)
	}
	if post, err := e.store.GetPost(ctx, got.ID); err != nil || post.Channel != "fake" || post.Ref.ID != "1" {
		t.Errorf("post record %+v %v", post, err)
	}
	if _, err := os.Stat(got.Worker.WorkspacePath); err != nil {
		t.Error("workspace must be kept while the run waits for review")
	}

	// Judge crash → uncertain → review, never pass.
	e.judge.fail = true
	tk2, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got2 := e.drive(t, tk2.ID)
	if got2.Status != task.StatusNeedsReview || got2.Eval.Judge.Verdict != "uncertain" || got2.Eval.Judge.Error == "" {
		t.Errorf("%s %+v", got2.Status, got2.Eval.Judge)
	}
	// A run that already failed checks never reaches the judge.
	e.judge.fail = false
	before := e.judge.calls
	tk3, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh fail.sh"))
	e.drive(t, tk3.ID)
	if e.judge.calls != before {
		t.Error("judge must be skipped when Layer 1 fails")
	}
}

func TestReviewApproveDeliversKeptWorkspace(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	e.judge.output = verdictJSON("uncertain", 8, false)
	ctx := context.Background()
	tk, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusNeedsReview {
		t.Fatalf("%s", got.Status)
	}
	// Wrong action on a non-reviewable run is rejected.
	if _, err := e.pool.Decide(ctx, review.Decision{RunID: "run_nope", Action: review.ActionApprove, By: "x"}); err == nil {
		t.Error("unknown run accepted")
	}
	out, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionApprove, By: "alice", Source: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.Status != task.StatusDelivered || len(out.Artifacts) != 1 || !strings.HasPrefix(out.Artifacts[0], "branch:harness/") || !strings.Contains(out.Message, "approved by api:alice") {
		t.Errorf("%+v", out)
	}
	if o := workspace.Git(t, e.origin, "log", "--oneline", "harness/"+tk.ID); !strings.Contains(o, "harness: Fix it") {
		t.Errorf("branch not pushed:\n%s", o)
	}
	if _, err := os.Stat(got.Worker.WorkspacePath); !os.IsNotExist(err) {
		t.Error("workspace not destroyed after delivery")
	}
	recs, _ := e.store.ListDecisions(ctx, got.ID)
	if len(recs) != 1 || recs[0].Decision.Action != review.ActionApprove || recs[0].JudgeVerdict != "uncertain" || recs[0].ChecksFailed {
		t.Errorf("decisions %+v", recs)
	}
	if post, _ := e.store.GetPost(ctx, got.ID); post.ResolvedAt == nil {
		t.Error("post not resolved")
	}
	if len(e.channel.resolved) != 1 || e.channel.resolved[0].By != "alice" {
		t.Errorf("channel not updated: %+v", e.channel.resolved)
	}
	// Second decision on the same run is refused.
	if _, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionClose, By: "bob"}); !errors.Is(err, review.ErrNotReviewable) {
		t.Errorf("err %v", err)
	}
}

func TestReviewRejectRetriesWithCommentAndCloseCloses(t *testing.T) {
	bin, rec := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	e.judge.output = verdictJSON("uncertain", 8, false)
	ctx := context.Background()
	tk, r1, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusNeedsReview {
		t.Fatalf("%s", got.Status)
	}
	out, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionReject, By: "alice", Comment: "Please also handle nil input", Source: "slack"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.Status != task.StatusClosed || out.NextRun == nil || out.NextRun.Attempt != 2 || out.NextRun.SessionMode != task.SessionContinue || !strings.Contains(out.Run.LastError, "rejected by alice") {
		t.Fatalf("%+v %+v", out.Run, out.NextRun)
	}
	if _, err := os.Stat(r1.Worker.WorkspacePath); !os.IsNotExist(err) {
		t.Error("kept workspace must be destroyed on reject")
	}
	// The retry runs (judge now passes) and delivers.
	e.judge.output = verdictJSON("pass", 9, false)
	final := e.drive(t, tk.ID)
	if final.ID != out.NextRun.ID || final.Status != task.StatusDelivered {
		t.Fatalf("final %+v", final)
	}
	if a2 := argv(t, rec, 2); !strings.Contains(a2, "Reviewer's note\nPlease also handle nil input") || !strings.Contains(a2, "--resume") {
		t.Errorf("retry prompt:\n%s", a2)
	}

	// Close.
	e.judge.output = verdictJSON("uncertain", 8, false)
	tk2, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got2 := e.drive(t, tk2.ID)
	out2, err := e.pool.Decide(ctx, review.Decision{RunID: got2.ID, Action: review.ActionClose, By: "bob", Comment: "not needed"})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Run.Status != task.StatusClosed || out2.NextRun != nil || !strings.Contains(out2.Run.LastError, "closed by bob: not needed") {
		t.Errorf("%+v", out2.Run)
	}
	if d, _ := e.q.Depth(ctx); d.Ready+d.Leased != 0 {
		t.Errorf("queue %+v", d)
	}
}

// planResult is a result event shaped like testdata/events/result_structured_output.json
// with a plan matching templates/code_fix_planned/phases/1 schema.
const planResult = `{"type":"result","subtype":"success","is_error":false,"duration_ms":2500,"duration_api_ms":2400,"num_turns":3,"result":"{\"summary\":\"Add Fixed\"}","session_id":"5b7d3f7e-3a1a-4f9e-9a5b-1e2d3c4b5a69","total_cost_usd":0.03,"usage":{"input_tokens":10,"output_tokens":50},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"tool_use","structured_output":{"summary":"Add Fixed to src/a.go","steps":[{"title":"Add function","files":["src/a.go"],"detail":"func Fixed() {}"}],"tests":["go test ./..."],"risks":["none"]},"uuid":"u"}`

func (e *env) plannedSpec() task.Spec {
	return task.Spec{Kind: task.KindCodeFixPlanned, Title: "Add Fixed", Prompt: "Add a Fixed function", Workspace: task.WorkspaceSpec{Type: "git", Repo: e.origin, Ref: "main"},
		Policy:     []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 1, "judge": {"enabled": false}}`),
		Acceptance: []byte(`{"commands": ["sh pass.sh"], "diff_scope": ["src/**"]}`), RequestedBy: "test"}
}

func TestMultiPhasePlanApproveImplement(t *testing.T) {
	bin, rec := fakeClaude(t, `if [ "$MODE" = "resume" ]; then printf 'package a\n\nfunc Fixed() {}\n' > src/a.go; cat "`+fixture("result_resumed.json")+`"; else echo '`+planResult+`'; fi`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r1, err := e.sub.Submit(ctx, e.plannedSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(tk.Phases) != 2 || tk.Phase != 1 || r1.Phase != 1 || tk.Phases[0].Name != "plan" || !tk.Phases[0].Review || tk.Phases[1].Prompt == "" {
		t.Fatalf("task phases %+v run %+v", tk.Phases, r1)
	}
	if p := tk.Phases[0].Policy; strings.Join(p.AllowedTools, ",") != "Read,Glob,Grep" || p.Judge.Enabled || p.MaxTurns != 15 || p.TimeoutMS != 5000 {
		t.Errorf("phase 1 policy %+v", p)
	}
	if a := tk.Phases[0].Acceptance; !a.HasSchema() || a.ExpectChanges == nil || *a.ExpectChanges || len(a.Commands) != 0 {
		t.Errorf("phase 1 acceptance %+v", a)
	}
	if a := tk.Phases[1].Acceptance; a.Commands[0] != "sh pass.sh" || a.ExpectChanges != nil {
		t.Errorf("phase 2 acceptance %+v", a)
	}

	// Phase 1: plan, no diff, schema valid → needs_review.
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusNeedsReview || !strings.Contains(got.LastError, "phase 1 (plan) requires approval") {
		t.Fatalf("%s %q eval=%+v", got.Status, got.LastError, got.Eval)
	}
	if got.Eval.Checks["schema_validation"].Status != "pass" || got.Eval.Checks["diff_sanity"].Status != "pass" || got.Eval.Checks["acceptance_commands"].Status != "n/a" {
		t.Errorf("phase 1 checks %+v", got.Eval.Checks)
	}
	if !strings.Contains(string(got.Output), "Add Fixed to src/a.go") {
		t.Errorf("plan not stored: %s", got.Output)
	}
	if a1 := argv(t, rec, 1); !strings.Contains(a1, "--json-schema") || !strings.Contains(a1, "--allowedTools\nRead,Glob,Grep") || !strings.Contains(a1, "--max-turns\n15") {
		t.Errorf("phase 1 argv:\n%s", a1)
	}
	if len(e.channel.posts) != 1 || e.channel.posts[0].Phase == nil || len(e.channel.posts[0].Output) == 0 {
		t.Errorf("review post %+v", e.channel.posts)
	}

	// Approve → phase 2 run resumes the session with the rendered phase prompt.
	out, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionApprove, By: "alice", Comment: "Keep it tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.Status != task.StatusDelivered || out.NextRun == nil || out.NextRun.Phase != 2 || out.NextRun.Attempt != 1 || out.NextRun.SessionMode != task.SessionContinue {
		t.Fatalf("%+v %+v", out.Run, out.NextRun)
	}
	if !contains(out.Run.Artifacts, "next:"+out.NextRun.ID) {
		t.Errorf("artifacts %v", out.Run.Artifacts)
	}
	tk2, _ := e.store.GetTask(ctx, tk.ID)
	if tk2.Phase != 2 || tk2.SessionID != r1.SessionID || out.NextRun.SessionID != tk2.SessionID {
		t.Errorf("task %+v next %+v", tk2, out.NextRun)
	}
	final := e.drive(t, tk.ID)
	if final.ID != out.NextRun.ID || final.Status != task.StatusDelivered || len(final.Artifacts) != 1 {
		t.Fatalf("final %+v", final)
	}
	a2 := argv(t, rec, 2)
	for _, want := range []string{"Plan approved", "Keep it tiny", `"summary": "Add Fixed to src/a.go"`, "--resume\n" + r1.SessionID, "--allowedTools\nRead,Edit,Write"} {
		if !strings.Contains(a2, want) {
			t.Errorf("phase 2 argv missing %q:\n%s", want, a2)
		}
	}
	if strings.Contains(a2, "--json-schema") {
		t.Error("phase 2 must not carry the plan schema")
	}
	if o := workspace.Git(t, e.origin, "log", "--oneline", "harness/"+tk.ID); !strings.Contains(o, "harness: Add Fixed") {
		t.Errorf("branch:\n%s", o)
	}
}

func TestMultiPhaseRejectReplans(t *testing.T) {
	bin, rec := fakeClaude(t, `echo '`+planResult+`'`)
	e := newEnv(t, bin)
	ctx := context.Background()
	tk, r1, _ := e.sub.Submit(ctx, e.plannedSpec())
	got := e.drive(t, tk.ID)
	out, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionReject, By: "alice", Comment: "Also add a test"})
	if err != nil {
		t.Fatal(err)
	}
	if out.NextRun.Phase != 1 || out.NextRun.Attempt != 2 || out.NextRun.SessionMode != task.SessionContinue || out.NextRun.RetryOf != r1.ID {
		t.Fatalf("%+v", out.NextRun)
	}
	got2 := e.drive(t, tk.ID)
	if got2.ID != out.NextRun.ID || got2.Status != task.StatusNeedsReview {
		t.Fatalf("%+v", got2)
	}
	if a2 := argv(t, rec, 2); !strings.Contains(a2, "Also add a test") || !strings.Contains(a2, "--json-schema") {
		t.Errorf("replan argv:\n%s", a2)
	}
	// A plan that edits files fails diff_sanity (read-only phase).
	bin2, _ := fakeClaude(t, `printf 'x' > src/a.go; echo '`+planResult+`'`)
	e2 := newEnv(t, bin2)
	tk3, _, _ := e2.sub.Submit(ctx, e2.plannedSpec())
	if got := e2.drive(t, tk3.ID); got.Eval.Checks["diff_sanity"].Status != "fail" || !strings.Contains(got.Eval.Checks["diff_sanity"].Evidence, "read-only") {
		t.Errorf("%+v", got.Eval.Checks["diff_sanity"])
	}
}

func TestEscalatorAndSessionSweeper(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin)
	e.judge.output = verdictJSON("uncertain", 8, false)
	ctx := context.Background()
	tk, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusNeedsReview {
		t.Fatal(got.Status)
	}
	future := time.Now().Add(48 * time.Hour)
	esc := &review.Escalator{Store: e.store, Reviews: e.store, Channel: e.channel, SLA: 24 * time.Hour, Logger: quiet, Now: func() time.Time { return future }}
	if n, err := esc.Once(ctx); err != nil || n != 1 || len(e.channel.escalated) != 1 || e.channel.escalated[0] != got.ID {
		t.Fatalf("n=%d err=%v escalated=%v", n, err, e.channel.escalated)
	}
	if n, _ := esc.Once(ctx); n != 0 {
		t.Error("escalated twice")
	}
	// Within the SLA nothing happens.
	tk2, _, _ := e.sub.Submit(ctx, e.judgedSpec("sh pass.sh"))
	e.drive(t, tk2.ID)
	esc.Now = time.Now
	if n, _ := esc.Once(ctx); n != 0 {
		t.Error("escalated inside the SLA")
	}

	// Sweeper: sessions of terminal tasks older than retention are deleted; live ones stay.
	if _, err := e.pool.Decide(ctx, review.Decision{RunID: got.ID, Action: review.ActionClose, By: "x"}); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(e.root, "sessions", tk.ID)
	if _, err := os.Stat(snap); err != nil {
		t.Fatal("snapshot missing before sweep")
	}
	sw := &SessionSweeper{Store: e.store, Sessions: e.sess, Retention: 30 * 24 * time.Hour, Logger: quiet, Now: func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }}
	if n, err := sw.Once(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Error("terminal task snapshot not deleted")
	}
	if _, err := os.Stat(filepath.Join(e.root, "sessions", tk2.ID)); err != nil {
		t.Error("non-terminal task snapshot deleted")
	}
}

func TestCrashFailsExitCode(t *testing.T) {
	bin, _ := fakeClaude(t, `printf 'package a\n// changed\n' > src/a.go; cat "`+fixture("stream_sigkill_mid_tool_use.ndjson")+`"; kill -9 $$`)
	e := newEnv(t, bin)
	ctx := context.Background()
	spec := e.spec()
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 5000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	tk, _, _ := e.sub.Submit(ctx, spec)
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDead || got.Eval.Checks["exit_code"].Status != "fail" || got.Metrics.TerminalReason != "crash" || got.Metrics.ExitCode != 137 {
		t.Errorf("%+v %+v", got, got.Eval)
	}
	if got.SessionURI == "" {
		t.Error("transcript must be snapshotted after a crash so a retry can resume it")
	}
}

func TestTimeoutKills(t *testing.T) {
	bin, _ := fakeClaude(t, `sleep 30`)
	e := newEnv(t, bin)
	spec := e.spec()
	spec.Policy = []byte(`{"max_turns": 5, "timeout_ms": 1500, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}}`)
	tk, _, _ := e.sub.Submit(context.Background(), spec)
	got := e.drive(t, tk.ID)
	if got.Status != task.StatusDead || !strings.Contains(got.Eval.Checks["exit_code"].Evidence, "killed=timeout") {
		t.Errorf("%s %+v", got.Status, got.Eval)
	}
}

func TestAPIErrorRequeuesWithoutConsumingAttemptThenDeadLetters(t *testing.T) {
	bin, _ := fakeClaude(t, `cat "`+fixture("result_not_logged_in.json")+`"; exit 1`)
	e := newEnv(t, bin) // MaxJobAttempts = 2
	ctx := context.Background()
	_, r, _ := e.sub.Submit(ctx, e.spec())

	if ok, err := e.pool.ProcessOne(ctx, ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	got, _ := e.store.GetRun(ctx, r.ID)
	if got.Status != task.StatusQueued || !strings.Contains(got.LastError, "api_error") || got.Attempt != 1 {
		t.Fatalf("after first api_error: %+v", got)
	}
	d, _ := e.q.Depth(ctx)
	if d.Ready != 1 {
		t.Fatalf("job not requeued: %+v", d)
	}
	time.Sleep(60 * time.Millisecond) // backoff
	if ok, err := e.pool.ProcessOne(ctx, ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	got, _ = e.store.GetRun(ctx, r.ID)
	if got.Status != task.StatusDead || !strings.Contains(got.LastError, "job attempts exhausted") {
		t.Fatalf("after second api_error: %+v", got)
	}
	d, _ = e.q.Depth(ctx)
	if d.Dead != 1 || d.Ready != 0 {
		t.Errorf("queue %+v", d)
	}
}

func TestProvisionFailureRequeues(t *testing.T) {
	bin, _ := fakeClaude(t, `exit 0`)
	e := newEnv(t, bin)
	ctx := context.Background()
	spec := e.spec()
	spec.Workspace.Ref = "no-such-branch"
	_, r, _ := e.sub.Submit(ctx, spec)
	if _, err := e.pool.ProcessOne(ctx, ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetRun(ctx, r.ID)
	if got.Status != task.StatusQueued || !strings.Contains(got.LastError, "provision workspace") {
		t.Errorf("%+v", got)
	}
}

func TestStaleJobForNonQueuedRunIsAcked(t *testing.T) {
	bin, _ := fakeClaude(t, `exit 0`)
	e := newEnv(t, bin)
	ctx := context.Background()
	_, r, _ := e.sub.Submit(ctx, e.spec())
	for _, s := range []task.RunStatus{task.StatusRunning, task.StatusEvaluating, task.StatusFailed, task.StatusDead} {
		if err := e.store.UpdateRunStatus(ctx, r.ID, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := e.pool.ProcessOne(ctx, ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if d, _ := e.q.Depth(ctx); d.Ready+d.Leased != 0 {
		t.Errorf("stale job not acked: %+v", d)
	}
}

func TestPerKindConcurrencyAndGracefulShutdown(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	bin, _ := fakeClaude(t, `while [ ! -f "`+gate+`" ]; do sleep 0.05; done; printf 'package a\n// x\n' > src/a.go; cat "`+fixture("result_success.json")+`"`)
	e := newEnv(t, bin) // Global 2, code_fix cap 1
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := e.sub.Submit(ctx, e.spec()); err != nil {
			t.Fatal(err)
		}
	}
	poolCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = e.pool.Run(poolCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, _ := e.store.ListRunsByStatus(ctx, task.StatusRunning)
		if len(running) == 1 {
			break
		}
		if len(running) > 1 {
			t.Fatalf("%d runs running; per-kind cap is 1", len(running))
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if running, _ := e.store.ListRunsByStatus(ctx, task.StatusRunning); len(running) != 1 {
		t.Fatalf("expected exactly 1 running, got %d", len(running))
	}
	cancel()
	if err := os.WriteFile(gate, []byte("go"), 0o640); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	delivered, _ := e.store.ListRunsByStatus(ctx, task.StatusDelivered)
	queued, _ := e.store.ListRunsByStatus(ctx, task.StatusQueued)
	if len(delivered) != 1 || len(queued) != 2 {
		t.Errorf("delivered=%d queued=%d (in-flight run must finish, others must stay queued)", len(delivered), len(queued))
	}
	if d, _ := e.q.Depth(ctx); d.Ready != 2 || d.Leased != 0 {
		t.Errorf("queue after shutdown %+v", d)
	}
}

func TestSubmitRejectsBadSpec(t *testing.T) {
	e := newEnv(t, "/nonexistent")
	ctx := context.Background()
	if _, _, err := e.sub.Submit(ctx, task.Spec{Kind: "nope", Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "unknown task kind") {
		t.Errorf("err %v", err)
	}
	if _, _, err := e.sub.Submit(ctx, task.Spec{Kind: task.KindCodeFix}); err == nil {
		t.Error("empty prompt accepted")
	}
	s := e.spec()
	s.Policy = []byte(`{"allowed_tools": []}`)
	if _, _, err := e.sub.Submit(ctx, s); err == nil {
		t.Error("empty allowlist accepted")
	}
	if _, err := e.q.Depth(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLeasableKinds(t *testing.T) {
	p := &Pool{Config: PoolConfig{PerKind: map[string]int{"code_fix": 1}}}
	p.init()
	if k := p.leasableKinds(context.Background()); k != nil {
		t.Errorf("nothing saturated should be nil, got %v", k)
	}
	rel := p.acquireKind("code_fix")
	if rel == nil {
		t.Fatal("acquire failed")
	}
	k := p.leasableKinds(context.Background())
	if len(k) == 0 || contains(k, "code_fix") || !contains(k, "report") {
		t.Errorf("saturated kinds: %v", k)
	}
	if p.acquireKind("code_fix") != nil {
		t.Error("second acquire should fail")
	}
	rel()
	if p.leasableKinds(context.Background()) != nil {
		t.Error("release should clear saturation")
	}
	if p.acquireKind("uncapped") == nil {
		t.Error("uncapped kind must always acquire")
	}
	_ = queue.ErrEmpty
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestAuthFailureAbortsAndRequeues(t *testing.T) {
	bin, _ := fakeClaude(t, `echo '{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"error_status":401,"error":"authentication_failed"}'; sleep 30`)
	e := newEnv(t, bin)
	ctx := context.Background()
	_, r, _ := e.sub.Submit(ctx, e.spec())
	start := time.Now()
	if ok, err := e.pool.ProcessOne(ctx, ctx); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	got, _ := e.store.GetRun(ctx, r.ID)
	if got.Status != task.StatusQueued || !strings.Contains(got.LastError, "api 401 authentication_failed") || !strings.Contains(got.LastError, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("%+v", got)
	}
	if d, _ := e.q.Depth(ctx); d.Ready != 1 {
		t.Errorf("not requeued: %+v", d)
	}
}

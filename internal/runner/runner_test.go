package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/session"
	"github.com/100xteam-ai/foreman/internal/task"
)

// fakeClaude writes a shell script standing in for the CLI. It records its argv
// and env, writes a transcript into $CLAUDE_CONFIG_DIR like the real CLI, and
// then runs `body`.
func fakeClaude(t *testing.T, body string) (bin, recordDir string) {
	t.Helper()
	dir := t.TempDir()
	recordDir = filepath.Join(dir, "rec")
	if err := os.MkdirAll(recordDir, 0o750); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
REC="` + recordDir + `"
printf '%s\n' "$@" > "$REC/argv"
env | sort > "$REC/env"
# find the session id after --session-id or --resume
SID=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--session-id" ] || { [ "$prev" = "--resume" ] && [ -z "$SID" ]; }; then SID="$a"; fi
  prev="$a"
done
# Like the CLI: append to an existing transcript found under any slug, else create one.
if [ -n "$CLAUDE_CONFIG_DIR" ] && [ -n "$SID" ]; then
  existing=$(ls "$CLAUDE_CONFIG_DIR"/projects/*/"$SID".jsonl 2>/dev/null | head -1)
  if [ -z "$existing" ]; then
    mkdir -p "$CLAUDE_CONFIG_DIR/projects/-fake-slug"
    existing="$CLAUDE_CONFIG_DIR/projects/-fake-slug/$SID.jsonl"
  fi
  echo '{"type":"user","text":"transcript"}' >> "$existing"
fi
` + body + "\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, recordDir
}

func fixturePath(name string) string {
	p, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "events", name))
	return p
}

func newRunner(t *testing.T, bin string) (*Runner, string) {
	t.Helper()
	root := t.TempDir()
	sess, err := session.NewLocal(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.NewFileSink(filepath.Join(root, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Bin: bin, APIKey: "sk-test", Sessions: sess, Audit: aud, Grace: 200 * time.Millisecond,
		ExtraEnv: []string{"GOFLAGS=-mod=mod", "HOME=/should/be/ignored"}}, root
}

func spawn(t *testing.T, root string) Spawn {
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	return Spawn{Workspace: ws, ConfigDir: filepath.Join(root, "cfg")}
}

func TestRunSuccessFixture(t *testing.T) {
	bin, rec := fakeClaude(t, `cat "`+fixturePath("stream_sigkill_mid_tool_use.ndjson")+`"; echo "not json"; cat "`+fixturePath("result_success.json")+`"; exit 0`)
	rn, root := newRunner(t, bin)
	tk, r := baseTask(), run(task.SessionNew)
	res, err := rn.Run(context.Background(), tk, r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != events.OutcomeCompleted || res.ExitCode != 0 || res.Result == nil || res.Result.Text() != "OK" {
		t.Errorf("result: %+v", res)
	}
	if res.Events != 8 || res.ToolUses != 1 || res.Killed != KillNone {
		t.Errorf("events=%d tools=%d killed=%q", res.Events, res.ToolUses, res.Killed)
	}
	if res.EstCostUSD <= 0 {
		t.Error("cost estimate not tracked")
	}
	m := res.Metrics()
	if m.CostUSD != 0.0196885 || m.Turns != 1 || m.TerminalReason != "completed" || m.DurationMS != 1969 {
		t.Errorf("metrics %+v", m)
	}

	// argv: prompt is one element, session flag present, no shell mangling.
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	lines := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
	if lines[0] != "-p" || lines[1] != tk.Prompt || lines[len(lines)-2] != "--session-id" || lines[len(lines)-1] != r.SessionID {
		t.Errorf("argv:\n%s", argv)
	}
	// env allowlist.
	env, _ := os.ReadFile(filepath.Join(rec, "env"))
	for _, want := range []string{"ANTHROPIC_API_KEY=sk-test", "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "cfg"), "GOFLAGS=-mod=mod", "PATH="} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env missing %s:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "HOME=/should/be/ignored") {
		t.Error("HOME override leaked into worker env")
	}
	for _, k := range []string{"TERM_PROGRAM", "SHELL=", "USER=", "CLAUDECODE"} {
		if strings.Contains(string(env), "\n"+k) {
			t.Errorf("inherited %s", k)
		}
	}
	// audit log has every stdout line including the raw one.
	log, _ := os.ReadFile(strings.TrimPrefix(res.EventLogURI, "file://"))
	if got := strings.Count(string(log), "\n"); got != 8 || !strings.Contains(string(log), "not json\n") {
		t.Errorf("audit lines=%d\n%s", got, log)
	}
	// transcript snapshotted.
	if res.SessionURI == "" {
		t.Fatal("no session snapshot")
	}
	if _, err := os.Stat(strings.TrimPrefix(res.SessionURI, "file://")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cfg")); err != nil {
		t.Error("config dir not created")
	}
}

func TestRunClassifiesFixtures(t *testing.T) {
	cases := []struct {
		fixture string
		exit    string
		outcome events.Outcome
	}{
		{"result_budget_exhausted.json", "1", events.OutcomeBudgetExhausted},
		{"result_not_logged_in.json", "1", events.OutcomeAPIError},
		{"result_resumed.json", "0", events.OutcomeCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			bin, _ := fakeClaude(t, `cat "`+fixturePath(tc.fixture)+`"; exit `+tc.exit)
			rn, root := newRunner(t, bin)
			res, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.outcome {
				t.Errorf("outcome %s want %s", res.Outcome, tc.outcome)
			}
			if tc.exit == "1" && res.ExitCode != 1 {
				t.Errorf("exit %d", res.ExitCode)
			}
		})
	}
}

func TestRunCrashWithoutResult(t *testing.T) {
	bin, _ := fakeClaude(t, `cat "`+fixturePath("stream_sigkill_mid_tool_use.ndjson")+`"; kill -9 $$`)
	rn, root := newRunner(t, bin)
	res, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != events.OutcomeCrash || !res.Signaled || res.ExitCode != 137 || res.Result != nil {
		t.Errorf("crash not detected: %+v", res)
	}
	if res.SessionURI == "" {
		t.Error("transcript must be snapshotted after a crash")
	}
	if m := res.Metrics(); m.TerminalReason != "crash" || m.CostUSD != 0 {
		t.Errorf("metrics %+v", m)
	}
}

func TestRunStderrUsageErrors(t *testing.T) {
	cases := []struct {
		fixture string
		check   func(*Result) bool
	}{
		{"stderr_no_conversation_found.txt", func(r *Result) bool { return r.SessionLost }},
		{"stderr_session_id_in_use.txt", func(r *Result) bool { return r.SessionIDInUse }},
		{"stderr_session_id_with_resume.txt", func(r *Result) bool { return !r.SessionLost && !r.SessionIDInUse }},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			bin, _ := fakeClaude(t, `cat "`+fixturePath(tc.fixture)+`" >&2; exit 1`)
			rn, root := newRunner(t, bin)
			res, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != events.OutcomeCrash || res.ExitCode != 1 || res.Events != 0 || !tc.check(res) {
				t.Errorf("%+v", res)
			}
			if !strings.Contains(res.Stderr, strings.TrimSpace(res.Stderr)) || res.Stderr == "" {
				t.Error("stderr not captured")
			}
		})
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	// The fake spawns a child that would outlive a pid-only kill.
	bin, rec := fakeClaude(t, `(sleep 30) & echo $! > "$REC/child"; sleep 30`)
	rn, root := newRunner(t, bin)
	tk := baseTask()
	tk.Policy.TimeoutMS = 1500
	start := time.Now()
	res, err := rn.Run(context.Background(), tk, run(task.SessionNew), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillTimeout || res.Outcome != events.OutcomeCrash || !res.Signaled {
		t.Errorf("timeout not applied: %+v", res)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("took %s", d)
	}
	childPid, _ := os.ReadFile(filepath.Join(rec, "child"))
	pid := strings.TrimSpace(string(childPid))
	if pid == "" {
		t.Fatal("child pid not recorded")
	}
	// Give the group signal a moment, then the child must be gone.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); err != nil { // linux fast path
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive(pid) {
		t.Errorf("tool child %s survived the group kill", pid)
	}
}

func alive(pid string) bool {
	// kill -0 via sh keeps this portable (macOS has no /proc).
	out, err := shell(`kill -0 ` + pid + ` 2>/dev/null && echo alive || echo dead`)
	return err == nil && strings.TrimSpace(out) == "alive"
}

func TestRunBudgetBackstop(t *testing.T) {
	// One assistant event priced far above the policy ceiling, then hang.
	line := `{"type":"assistant","message":{"id":"m1","model":"claude-opus-4-1","usage":{"input_tokens":0,"output_tokens":2000000}}}`
	bin, _ := fakeClaude(t, `echo '`+line+`'; sleep 30`)
	rn, root := newRunner(t, bin)
	tk := baseTask()
	tk.Policy.MaxCostUSD = 1
	res, err := rn.Run(context.Background(), tk, run(task.SessionNew), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillBudget || res.EstCostUSD < 100 {
		t.Errorf("backstop not applied: killed=%q est=%v", res.Killed, res.EstCostUSD)
	}
}

func TestRunContextCancel(t *testing.T) {
	bin, _ := fakeClaude(t, `sleep 30`)
	rn, root := newRunner(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res, err := rn.Run(ctx, baseTask(), run(task.SessionNew), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillCanceled {
		t.Errorf("killed=%q", res.Killed)
	}
}

func TestRunContinueRestoresTranscript(t *testing.T) {
	bin, rec := fakeClaude(t, `ls "$CLAUDE_CONFIG_DIR"/projects/*/ > "$REC/restored"; cat "`+fixturePath("result_resumed.json")+`"`)
	rn, root := newRunner(t, bin)
	tk := baseTask()
	sid := "d1ba9815-1ddf-4c2c-bdfc-a673f5a69b64"
	// Seed a snapshot as if attempt 1 had run.
	snapDir := filepath.Join(root, "sessions", tk.ID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, sid+".jsonl"), []byte("{\"seed\":true}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := run(task.SessionContinue)
	r.SessionID = sid
	res, err := rn.Run(context.Background(), tk, r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(filepath.Join(rec, "restored"))
	if !strings.Contains(string(restored), sid+".jsonl") {
		t.Errorf("transcript not restored before spawn: %s", restored)
	}
	if res.Outcome != events.OutcomeCompleted || res.SessionURI == "" {
		t.Errorf("%+v", res)
	}
	snap, _ := os.ReadFile(strings.TrimPrefix(res.SessionURI, "file://"))
	if !strings.Contains(string(snap), "transcript") || !strings.Contains(string(snap), "seed") {
		t.Errorf("snapshot should contain the restored + appended transcript: %s", snap)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if !strings.Contains(string(argv), "--resume\n"+sid) || strings.Contains(string(argv), "--session-id") {
		t.Errorf("continue argv wrong:\n%s", argv)
	}
}

func TestRunContinueWithoutSnapshotIsSessionLost(t *testing.T) {
	bin, rec := fakeClaude(t, `exit 0`)
	rn, root := newRunner(t, bin)
	r := run(task.SessionContinue)
	res, err := rn.Run(context.Background(), baseTask(), r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if !res.SessionLost || res.Outcome != events.OutcomeCrash {
		t.Errorf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(rec, "argv")); err == nil {
		t.Error("CLI must not be spawned when the transcript cannot be restored")
	}
}

func TestRunEphemeralLeavesNoSnapshot(t *testing.T) {
	bin, _ := fakeClaude(t, `cat "`+fixturePath("result_success.json")+`"`)
	rn, root := newRunner(t, bin)
	res, err := rn.Run(context.Background(), baseTask(), run(task.SessionEphemeral), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionURI != "" {
		t.Error("ephemeral run must not snapshot")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "sessions")); len(entries) != 0 {
		t.Error("session store not empty")
	}
}

func TestRunPromptViaStdin(t *testing.T) {
	bin, rec := fakeClaude(t, `cat > "$REC/stdin"; cat "`+fixturePath("result_success.json")+`"`)
	rn, root := newRunner(t, bin)
	rn.Args.PromptViaStdin = true
	tk := baseTask()
	if _, err := rn.Run(context.Background(), tk, run(task.SessionNew), spawn(t, root)); err != nil {
		t.Fatal(err)
	}
	in, _ := os.ReadFile(filepath.Join(rec, "stdin"))
	if string(in) != tk.Prompt {
		t.Errorf("stdin %q", in)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if strings.Contains(string(argv), tk.Prompt) {
		t.Error("prompt must not be in argv when PromptViaStdin")
	}
}

func TestRunMissingBinary(t *testing.T) {
	rn, root := newRunner(t, filepath.Join(t.TempDir(), "nope"))
	if _, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root)); err == nil {
		t.Error("missing binary should error")
	}
}

const apiRetry401 = `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":586,"error_status":401,"error":"authentication_failed","session_id":"s","uuid":"u"}`

func TestRunAuthFailureAbortsEarly(t *testing.T) {
	// The real CLI retries a 401 ten times (~3 minutes). The runner must not wait.
	bin, _ := fakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"s"}'; echo '`+apiRetry401+`'; sleep 30`)
	rn, root := newRunner(t, bin)
	start := time.Now()
	res, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillAuth || !res.AuthFailed || res.Outcome != events.OutcomeAPIError || res.AuthError != "api 401 authentication_failed" {
		t.Errorf("%+v", res)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s; should abort on the first 401", d)
	}
	// A 5xx retry is transient and must not abort.
	bin, _ = fakeClaude(t, `echo '{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"error_status":529,"error":"overloaded"}'; cat "`+fixturePath("result_success.json")+`"`)
	rn, root = newRunner(t, bin)
	res, err = rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root))
	if err != nil || res.Killed != KillNone || res.Outcome != events.OutcomeCompleted {
		t.Errorf("529 retry aborted the run: %+v %v", res, err)
	}
}

func TestRunOAuthTokenEnv(t *testing.T) {
	bin, rec := fakeClaude(t, `cat "`+fixturePath("result_success.json")+`"`)
	rn, root := newRunner(t, bin)
	rn.APIKey = ""
	rn.OAuthToken = "sk-ant-oat01-test"
	rn.ExtraEnv = append(rn.ExtraEnv, "ANTHROPIC_API_KEY=should-not-leak-via-extra-env")
	if _, err := rn.Run(context.Background(), baseTask(), run(task.SessionNew), spawn(t, root)); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(rec, "env"))
	if !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-test") {
		t.Errorf("oauth token not passed:\n%s", env)
	}
	if strings.Contains(string(env), "ANTHROPIC_API_KEY=") {
		t.Error("credentials must only come from Runner fields, not ExtraEnv")
	}
}

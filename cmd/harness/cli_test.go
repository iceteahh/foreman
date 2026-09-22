package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// capture redirects os.Stdout while fn runs and returns what it printed. The
// ops commands write to stdout directly, which is their whole output.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			b.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		done <- b.String()
	}()
	fnErr := fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, fnErr
}

// deadRun drives a task that fails every attempt, so the ops commands have a
// real dead-letter entry to work with.
func deadRun(t *testing.T, cfgPath string) (taskID, runID string) {
	t.Helper()
	origin := bareRepo(t)
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// The worker edits nothing, so `check.sh` fails on every attempt.
	spec := fmt.Sprintf(`{
	  "kind": "code_fix",
	  "prompt": "do nothing",
	  "workspace": {"type": "git", "repo": %q, "ref": "main"},
	  "policy": {"max_turns": 3, "timeout_ms": 20000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}},
	  "acceptance": {"commands": ["sh check.sh"], "diff_scope": ["**"]}
	}`, origin)
	run, err := a.RunOnce(t.Context(), []byte(spec))
	if err != nil {
		t.Fatalf("run-once: %v", err)
	}
	if string(run.Status) != "dead" {
		t.Fatalf("expected a dead run to work with, got %s (%q)", run.Status, run.LastError)
	}
	return run.TaskID, run.ID
}

// The operator's recovery loop, as an operator runs it: see the dead letter,
// read the timeline, look at the task tree, then requeue.
func TestOpsCommandsOnADeadRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	bin := fakeClaude(t, `cat "`+fixture(t, "result_success.json")+`"`)
	cfgPath, _ := writeConfig(t, bin)
	taskID, runID := deadRun(t, cfgPath)
	cfg := []string{"-config", cfgPath}

	out, err := capture(t, func() error { return cmdRequeue(append(cfg, "-list"), quiet()) })
	if err != nil {
		t.Fatalf("requeue -list: %v", err)
	}
	if !strings.Contains(out, runID) || !strings.Contains(out, "REASON") {
		t.Errorf("the dead letter is not listed:\n%s", out)
	}

	out, err = capture(t, func() error { return cmdReplay(append(cfg, runID), quiet()) })
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !strings.Contains(out, runID) || !strings.Contains(out, "dead") {
		t.Errorf("the replay does not show the run's fate:\n%s", out)
	}

	out, err = capture(t, func() error { return cmdTree(append(cfg, taskID), quiet()) })
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if !strings.Contains(out, taskID) || !strings.Contains(out, runID) {
		t.Errorf("the tree does not show the task and its run:\n%s", out)
	}

	out, err = capture(t, func() error { return cmdBudget(cfg, quiet()) })
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	if !strings.Contains(out, "KEY") || !strings.Contains(out, "global") {
		t.Errorf("budget does not report the ceilings:\n%s", out)
	}

	// Requeue gives the run a fresh attempt counter and closes the entry.
	out, err = capture(t, func() error {
		return cmdRequeue(append(cfg, "-note", "create the file", runID), quiet())
	})
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if !strings.Contains(out, "requeued "+runID) {
		t.Errorf("requeue said nothing useful:\n%s", out)
	}
	var next map[string]any
	if i := strings.Index(out, "{"); i >= 0 {
		_ = json.Unmarshal([]byte(out[i:]), &next)
	}
	if next["attempt"] == nil {
		t.Errorf("requeue did not print the new run:\n%s", out)
	}
	out, err = capture(t, func() error { return cmdRequeue(append(cfg, "-list"), quiet()) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no open dead letters") {
		t.Errorf("the entry stayed open after a requeue:\n%s", out)
	}
}

// Bad arguments fail with a message rather than a panic or a silent no-op.
func TestOpsCommandsRejectBadArguments(t *testing.T) {
	cfgPath, _ := writeConfig(t, "")
	cfg := []string{"-config", cfgPath}
	for name, fn := range map[string]func() error{
		"replay without a run":  func() error { return cmdReplay(cfg, quiet()) },
		"replay with two runs":  func() error { return cmdReplay(append(cfg, "a", "b"), quiet()) },
		"tree without a task":   func() error { return cmdTree(cfg, quiet()) },
		"requeue without a run": func() error { return cmdRequeue(cfg, quiet()) },
		"replay unknown run":    func() error { return cmdReplay(append(cfg, "run_nope"), quiet()) },
		"tree unknown task":     func() error { return cmdTree(append(cfg, "tsk_nope"), quiet()) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := capture(t, fn); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// replay -file reads a captured stream with no store at all, which is how a
// fixture is inspected and how the demos show a timeline.
func TestReplayFromAFixtureFile(t *testing.T) {
	out, err := capture(t, func() error {
		return cmdReplay([]string{"-file", fixture(t, "stream_structured_output.ndjson"), "-v"}, quiet())
	})
	if err != nil {
		t.Fatalf("replay -file: %v", err)
	}
	if !strings.Contains(out, "result") {
		t.Errorf("the replay printed no timeline:\n%s", out)
	}
	if _, err := capture(t, func() error { return cmdReplay([]string{"-file", "/no/such/log.ndjson"}, quiet()) }); err == nil {
		t.Error("a missing file was accepted")
	}
}

// `eval list` and `eval gate` are the free half of Layer 4: they must work
// without a worker credential, because CI runs them on every pull request.
func TestEvalListAndGateAreFree(t *testing.T) {
	suite, err := filepath.Abs(filepath.Join("..", "..", "evals", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, func() error { return cmdEval([]string{"list", suite}, quiet()) })
	if err != nil {
		t.Fatalf("eval list: %v", err)
	}
	if !strings.Contains(out, "case(s) in") {
		t.Errorf("eval list printed no cases:\n%s", out)
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const good = `{"suite":"s","totals":{"cases":2,"passed":2,"failed":0,"scored":2,"pass_rate":1},"cases":[
	  {"id":"a","ok":true},{"id":"b","ok":true}]}`
	const worse = `{"suite":"s","totals":{"cases":2,"passed":1,"failed":1,"scored":2,"pass_rate":0.5},"cases":[
	  {"id":"a","ok":false},{"id":"b","ok":true}]}`
	base, cur := write("baseline.json", good), write("run.json", worse)

	if _, err := capture(t, func() error {
		return cmdEval([]string{"gate", "-baseline", base, "-report", cur}, quiet())
	}); err == nil {
		t.Error("the gate accepted a 50-point drop")
	}
	if _, err := capture(t, func() error {
		return cmdEval([]string{"gate", "-baseline", base, "-report", base}, quiet())
	}); err != nil {
		t.Errorf("the gate blocked an unchanged suite: %v", err)
	}
	// An absolute floor blocks a run that matches its baseline but is bad.
	if _, err := capture(t, func() error {
		return cmdEval([]string{"gate", "-baseline", cur, "-report", cur, "-min-pass-rate", "0.8"}, quiet())
	}); err == nil {
		t.Error("the floor did not block a 50% suite")
	}
	if _, err := capture(t, func() error { return cmdEval([]string{"gate"}, quiet()) }); err == nil {
		t.Error("gate without a report was accepted")
	}
	if _, err := capture(t, func() error { return cmdEval([]string{"nonsense"}, quiet()) }); err == nil {
		t.Error("an unknown eval subcommand was accepted")
	}
}

// doctor reports on the configured worker mode and fails when a check fails,
// which is what makes it usable as a pre-flight in a deploy script.
func TestDoctorReportsAndFailsLoudly(t *testing.T) {
	bin := fakeClaude(t, `echo "unexpected"`)
	cfgPath, _ := writeConfig(t, bin)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("HARNESS_API_TOKEN", "t0k3n")

	out, err := capture(t, func() error { return cmdDoctor([]string{"-config", cfgPath}) })
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	for _, want := range []string{"go", "git", "config", "claude", "credential", "api auth", "data root"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor did not check %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "HARNESS_API_TOKEN set") {
		t.Errorf("doctor does not report the API token:\n%s", out)
	}

	// No credential and an open API on a routable address: both must FAIL.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("HARNESS_API_TOKEN", "")
	open := strings.Replace(cfgPath, "harness.yaml", "open.yaml", 1)
	body, _ := os.ReadFile(cfgPath) //nolint:gosec // test path
	if err := os.WriteFile(open, []byte(strings.Replace(string(body), `"127.0.0.1:0"`, `":8080"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = capture(t, func() error { return cmdDoctor([]string{"-config", open}) })
	if err == nil {
		t.Error("doctor passed with no credential and an open API")
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "api auth") {
		t.Errorf("doctor did not flag the open API:\n%s", out)
	}
}

// The argument helpers decide what a command actually runs, and Go's flag
// package stops at the first positional — so `eval run <dir> -config x` needs
// the directory lifted out first or -config is silently ignored.
func TestArgumentHelpers(t *testing.T) {
	dir, rest := takeDir([]string{"evals/mine", "-config", "x"}, "evals/golden")
	if dir != "evals/mine" || len(rest) != 2 {
		t.Errorf("takeDir = %q %v", dir, rest)
	}
	if dir, rest := takeDir([]string{"-config", "x"}, "evals/golden"); dir != "evals/golden" || len(rest) != 2 {
		t.Errorf("takeDir with no positional = %q %v", dir, rest)
	}
	if got := splitList(" a , ,b "); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitList = %v", got)
	}
	if got := splitList(""); got != nil {
		t.Errorf("splitList(\"\") = %v, want nil", got)
	}
	if got := truncate("a\nvery long line indeed", 10); len([]rune(got)) > 10 || strings.Contains(got, "\n") {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate left a short string alone as %q", got)
	}
	if got := clipReason(strings.Repeat("x", 200)); len([]rune(got)) != 70 {
		t.Errorf("clipReason produced %d runes, want 70", len([]rune(got)))
	}
	if got := firstNonEmptyLine("\n\n  hello \nworld"); got != "hello" {
		t.Errorf("firstNonEmptyLine = %q", got)
	}
	if warnMark(true) != "ok  " || warnMark(false) != "warn" {
		t.Error("warnMark")
	}
	if configDetail(nil) != "" || !strings.Contains(configDetail(os.ErrNotExist), "not exist") {
		t.Error("configDetail")
	}
	t.Setenv("HARNESS_TEST_DETAIL", "")
	if got := envDetail("HARNESS_TEST_DETAIL", "nothing happens"); !strings.Contains(got, "nothing happens") {
		t.Errorf("envDetail = %q", got)
	}
	t.Setenv("HARNESS_TEST_DETAIL", "x")
	if got := envDetail("HARNESS_TEST_DETAIL", "nothing happens"); got != " set" {
		t.Errorf("envDetail = %q", got)
	}
}

// HARNESS_LOG picks the level; an unknown value falls back to info rather than
// silencing the harness.
func TestLogLevel(t *testing.T) {
	for value, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn,
		"error": slog.LevelError, "": slog.LevelInfo, "nonsense": slog.LevelInfo,
	} {
		t.Setenv("HARNESS_LOG", value)
		if got := logLevel(); got != want {
			t.Errorf("HARNESS_LOG=%q → %v, want %v", value, got, want)
		}
	}
}

// The non-Slack review path: a run the judge could not vouch for waits for a
// human, and `harness review` is how that human decides without Slack. An
// install with no Slack app has no other way to release a held run.
func TestReviewCommandDecidesAHeldRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := bareRepo(t)
	// The fake CLI satisfies the acceptance check but is not a judge: the
	// judge run gets the same canned result, cannot read a verdict from it,
	// and returns `uncertain` — which routes to a human, by design.
	bin := fakeClaude(t, `touch fixed; cat "`+fixture(t, "result_success.json")+`"`)
	cfgPath, _ := writeConfig(t, bin)
	cfg := []string{"-config", cfgPath}

	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`{
	  "kind": "code_fix",
	  "prompt": "create fixed",
	  "workspace": {"type": "git", "repo": %q, "ref": "main"},
	  "policy": {"max_turns": 3, "timeout_ms": 20000, "max_cost_usd": 1, "max_retries": 0,
	             "judge": {"enabled": true, "samples": 1, "threshold": 7}},
	  "acceptance": {"commands": ["sh check.sh"], "diff_scope": ["**"]}
	}`, origin)
	run, err := a.RunOnce(t.Context(), []byte(spec))
	a.Close()
	if err != nil {
		t.Fatalf("run-once: %v", err)
	}
	if string(run.Status) != "needs_review" {
		t.Skipf("this build did not produce a held run (%s); nothing to decide", run.Status)
	}

	// -run and exactly one action are required, and the action is checked.
	for name, args := range map[string][]string{
		"no run id":      append(cfg, "approve"),
		"no action":      append(cfg, "-run", run.ID),
		"unknown action": append(cfg, "-run", run.ID, "maybe"),
	} {
		if _, err := capture(t, func() error { return cmdReview(args, quiet()) }); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	out, err := capture(t, func() error {
		return cmdReview(append(cfg, "-run", run.ID, "-by", "tester", "-comment", "looks right", "approve"), quiet())
	})
	if err != nil {
		t.Fatalf("review approve: %v", err)
	}
	if !strings.Contains(out, run.ID) {
		t.Errorf("the decision does not name the run:\n%s", out)
	}

	// The run left needs_review, and the decision is on the record.
	b, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	after, err := b.Store.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Status) == "needs_review" {
		t.Errorf("the run is still held after an approval: %s (%q)", after.Status, after.LastError)
	}
	decisions, err := b.Store.ListDecisions(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Decision.By != "tester" || decisions[0].Decision.Source == "" {
		t.Errorf("the audit trail does not record who decided: %+v", decisions)
	}
	// Deciding an already-decided run is refused rather than applied twice.
	if _, err := capture(t, func() error {
		return cmdReview(append(cfg, "-run", run.ID, "approve"), quiet())
	}); err == nil {
		t.Error("a second decision on the same run was accepted")
	}
}

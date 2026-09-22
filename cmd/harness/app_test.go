package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeClaude writes a stand-in CLI that replays a captured result event. The
// harness never knows the difference: it reads NDJSON on stdout, which is the
// whole contract (docs/cli-contract.md).
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.270 (Claude Code)'; exit 0; fi\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeConfig produces a harness.yaml whose data_root is a temp directory.
// data_root must sit outside any git repository, which is exactly what
// config.Load enforces and why the test cannot write it under the checkout.
func writeConfig(t *testing.T, claudeBin string) (cfgPath, dataRoot string) {
	t.Helper()
	dir := t.TempDir()
	dataRoot = filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "harness.yaml")
	if claudeBin == "" {
		claudeBin = "claude"
	}
	body := fmt.Sprintf(`
data_root: %q
server: { addr: "127.0.0.1:0" }
worker: { mode: local, check_version: false, claude_bin: %q }
observability: { enabled: false, progress: "off", prometheus_addr: "" }
review: { slack_channel: "", sla_hours: 0 }
retention: { sessions_days: 0 }
`, dataRoot, claudeBin)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataRoot
}

// buildApp creates every directory the harness writes into, so a first boot on
// a clean machine does not fail on the first run instead of at startup.
func TestBuildAppCreatesItsDataRoot(t *testing.T) {
	cfgPath, dataRoot := writeConfig(t, "")
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	for _, d := range []string{dataRoot, a.Config.AuditRoot(), a.Config.WorkspaceRoot(),
		a.Config.ConfigDirRoot(), a.Config.SessionRoot(), a.Config.ReportRoot()} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Errorf("%s was not created: %v", d, err)
		}
	}
	if a.Store == nil || a.Queue == nil || a.Pool == nil || a.HTTP == nil || a.Cron == nil {
		t.Errorf("app is missing a component: %+v", a)
	}
	if a.Replica == "" {
		t.Error("no replica id; periodic leases would collide")
	}
	// The stranded-run sweeper and the fan-in sweep are both registered: a
	// harness with no background jobs loses every recovery path.
	if len(a.Background) == 0 {
		t.Error("no periodic jobs registered")
	}
}

// A data_root inside a git repository silently feeds that repository's CLAUDE.md
// to every worker, so startup refuses it rather than producing quietly wrong runs.
func TestBuildAppRefusesDataRootInsideARepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "harness.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_root: ./data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildApp(cfgPath, quiet()); err == nil {
		t.Fatal("a data_root inside a repository was accepted")
	} else if !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("the error does not explain itself: %v", err)
	}
}

// serveAt starts Serve on a real listener and returns its base URL plus a stop
// function that cancels and waits for the drain, the way SIGTERM does.
func serveAt(t *testing.T, a *app) (base string, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- a.Serve(ctx) }()

	// Serve picks the port (addr ends in :0), so discover it by polling.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if p := listeningPort(t, a); p != "" {
			base = "http://127.0.0.1:" + p
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("Serve never started listening: %v", <-errc)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("Serve did not drain within 30s of cancellation")
			return nil
		}
	}
}

// listeningPort finds the port Serve bound, by asking the OS for this process's
// listening sockets. addr :0 means the config cannot tell us.
func listeningPort(t *testing.T, a *app) string {
	t.Helper()
	out, err := exec.Command("lsof", "-a", "-p", fmt.Sprint(os.Getpid()), "-iTCP", "-sTCP:LISTEN", "-P", "-n").Output()
	if err != nil {
		t.Skipf("cannot discover the listening port (lsof): %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "127.0.0.1:") {
			continue
		}
		i := strings.LastIndex(line, "127.0.0.1:")
		port := strings.Fields(line[i+len("127.0.0.1:"):])[0]
		port = strings.TrimSuffix(port, "(LISTEN)")
		if port != "" {
			return port
		}
	}
	return ""
}

// Serve boots, answers /healthz, and drains cleanly when its context ends —
// which is what a SIGTERM does to the real binary. A harness that does not
// return here leaves workers running and jobs leased on every deploy.
func TestServeBootsAndDrainsOnSignal(t *testing.T) {
	cfgPath, _ := writeConfig(t, "")
	t.Setenv("HARNESS_API_TOKEN", "")
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	base, stop := serveAt(t, a)
	resp, err := http.Get(base + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&health)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || health["ok"] != true {
		t.Errorf("/healthz = %d %v", resp.StatusCode, health)
	}
	if health["queue"] == nil {
		t.Error("/healthz does not report queue depth, so it cannot serve as a readiness probe")
	}

	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Serve returned an error on a clean shutdown: %v", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("the drain took %s", d)
	}
	// The listener really is closed afterwards.
	if _, err := http.Get(base + "/healthz"); err == nil { //nolint:noctx // test
		t.Error("the listener survived the drain")
	}
}

// The API carries write-capable task submission and review decisions. With a
// token configured every route needs it, except /healthz and the HMAC-verified
// webhook — and this is the wiring through `serve`, not just the mux.
func TestServeEnforcesTheAPIToken(t *testing.T) {
	cfgPath, _ := writeConfig(t, "")
	t.Setenv("HARNESS_API_TOKEN", "t0k3n")
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.HTTP.Token != "t0k3n" {
		t.Fatalf("the token never reached the server: %q", a.HTTP.Token)
	}
	base, stop := serveAt(t, a)
	defer func() { _ = stop() }()

	req, _ := http.NewRequest(http.MethodGet, base+"/runs?status=queued", nil) //nolint:noctx // test
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /runs without a token = %d, want 401", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer t0k3n")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /runs with the token = %d, want 200", resp.StatusCode)
	}
	// The probe stays open, or the liveness check fails every deploy.
	resp, err = http.Get(base + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz needs a token (%d); probes cannot send one", resp.StatusCode)
	}
}

// An unauthenticated API on a routable address is a startup failure, not a
// warning: anyone who reaches the port could submit a write-capable task or
// approve any run.
func TestServeRefusesAnOpenAPIOnARoutableAddress(t *testing.T) {
	cfgPath, _ := writeConfig(t, "")
	t.Setenv("HARNESS_API_TOKEN", "")
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Config.Server.Addr = "0.0.0.0:0"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = a.Serve(ctx)
	if err == nil {
		t.Fatal("serve started with an open API on a routable address")
	}
	if !strings.Contains(err.Error(), "HARNESS_API_TOKEN") {
		t.Errorf("the refusal does not say what to set: %v", err)
	}
}

// run-once end to end against a fake CLI: submit, run, check, deliver. This is
// the path every demo and the golden suite executor go through.
func TestRunOnceDriverDeliversATask(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := bareRepo(t)
	bin := fakeClaude(t, `touch fixed; cat "`+fixture(t, "result_success.json")+`"`)
	cfgPath, _ := writeConfig(t, bin)
	a, err := buildApp(cfgPath, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	spec := fmt.Sprintf(`{
	  "kind": "code_fix",
	  "prompt": "create fixed",
	  "workspace": {"type": "git", "repo": %q, "ref": "main"},
	  "policy": {"max_turns": 3, "timeout_ms": 20000, "max_cost_usd": 1, "max_retries": 0, "judge": {"enabled": false}},
	  "acceptance": {"commands": ["sh check.sh"], "diff_scope": ["**"]}
	}`, origin)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	run, err := a.RunOnce(ctx, []byte(spec))
	if err != nil {
		t.Fatalf("run-once: %v", err)
	}
	if run.Status != task.StatusDelivered {
		t.Fatalf("run ended %s (%q)", run.Status, run.LastError)
	}
	if run.Metrics.CostUSD <= 0 {
		t.Error("no cost recorded, so the budget ledger saw nothing")
	}
	// A malformed spec is rejected before anything is created.
	if _, err := a.RunOnce(ctx, []byte(`{"kind":"nope"}`)); err == nil {
		t.Error("an unknown kind was accepted")
	}
	if _, err := a.RunOnce(ctx, []byte(`{"typo": 1}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}

// bareRepo builds a throwaway origin the harness can clone and push to.
func bareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"README.md": "# demo\n",
		// Passes once the worker has created `fixed`.
		"check.sh": "#!/bin/sh\n[ -f fixed ] && exit 0\necho 'fixed missing' >&2; exit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:gosec // test argv
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(work, "init", "--quiet", "-b", "main")
	git(work, "config", "user.name", "test")
	git(work, "config", "user.email", "test@example.invalid")
	git(work, "config", "commit.gpgsign", "false")
	git(work, "add", "-A")
	git(work, "commit", "--quiet", "-m", "initial")
	origin := filepath.Join(dir, "origin.git")
	git(dir, "clone", "--quiet", "--bare", work, origin)
	return origin
}

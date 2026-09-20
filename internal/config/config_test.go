package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultsValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.DraftPRs() || !c.CheckVersion() || c.Concurrency.PerKind["code_fix"] != 2 || c.Worker.OAuthTokenEnv != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("%+v", c)
	}
}

func TestLoadOverridesAndPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "harness.yaml")
	body := `
data_root: data
concurrency: { global: 4, per_kind: { code_fix: 3 } }
worker: { claude_bin: /opt/claude, budget_flag_ratio: 0.8, check_version: false }
github: { draft_prs: false, trigger_label: "" }
env: { GOFLAGS: "-mod=mod", SECRET: "$HARNESS_TEST_SECRET", EMPTY: "$HARNESS_TEST_MISSING" }
cron:
  - name: nightly
    schedule: "0 2 * * *"
    task: { kind: report, prompt: "hi", workspace: { type: git, repo: o/r, ref: main } }
`
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_TEST_SECRET", "s3")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// yaml.v3 merges maps into the defaults: unspecified kinds keep their default cap.
	if c.Concurrency.Global != 4 || c.Concurrency.PerKind["code_fix"] != 3 || c.Concurrency.PerKind["report"] != 1 {
		t.Errorf("concurrency %+v", c.Concurrency)
	}
	if c.Worker.ClaudeBin != "/opt/claude" || c.Worker.BudgetFlagRatio != 0.8 || c.CheckVersion() || c.Worker.DefaultMaxTurns != 30 {
		t.Errorf("worker %+v", c.Worker)
	}
	if c.DraftPRs() || c.GitHub.TriggerLabel != "" || c.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("github %+v", c.GitHub)
	}
	if c.DataRoot != filepath.Join(dir, "data") || c.DBPath() != filepath.Join(dir, "data", "harness.db") || c.SessionRoot() != filepath.Join(dir, "data", "sessions") {
		t.Errorf("paths %s %s", c.DataRoot, c.DBPath())
	}
	env := strings.Join(c.WorkerEnv(), ";")
	if !strings.Contains(env, "GOFLAGS=-mod=mod") || !strings.Contains(env, "SECRET=s3") || strings.Contains(env, "EMPTY") {
		t.Errorf("env %s", env)
	}
	if len(c.Cron) != 1 || c.Cron[0].Task["kind"] != "report" {
		t.Errorf("cron %+v", c.Cron)
	}
}

// Regression: check_version and draft_prs defaults used to share one *bool.
func TestBoolDefaultsAreIndependent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.yaml")
	if err := os.WriteFile(p, []byte("worker: { check_version: false }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.CheckVersion() {
		t.Error("check_version not applied")
	}
	if !c.DraftPRs() {
		t.Error("draft_prs flipped by check_version (shared pointer)")
	}
	if Default().DraftPRs() != true || Default().CheckVersion() != true {
		t.Error("defaults changed")
	}
}

func TestLoadRejects(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field":   "nope: 1\n",
		"bad ratio":       "worker: { budget_flag_ratio: 2 }\n",
		"docker no image": "worker: { mode: docker, image: \"\" }\n",
		"bad user":        "worker: { mode: docker, docker: { user: \"a b\" } }\n",
		"cron":            "cron: [ { name: x } ]\n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".yaml")
		_ = os.WriteFile(p, []byte(body), 0o640)
		if _, err := Load(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err != nil {
		t.Errorf("missing file should yield defaults: %v", err)
	}
}

// Docker mode is configured, not rejected, since plan Step 14.
func TestDockerModeConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	body := "worker:\n  mode: docker\n  image: harness/worker:test\n  docker:\n    network: harness-internal\n    user: \"\"\n    egress_proxy: http://egress:3128\n    memory: 2g\n    cpus: 1.5\n    pids_limit: 256\n    read_only_root: true\n    kill_timeout_ms: 30000\negress:\n  addr: \":3129\"\n  allow: [api.anthropic.com]\n  ports: [443]\n"
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	o := c.ContainerOptions()
	if o.Image != "harness/worker:test" || o.Network != "harness-internal" || o.EgressProxy != "http://egress:3128" ||
		o.Memory != "2g" || o.CPUs != 1.5 || o.PidsLimit != 256 || !o.ReadOnlyRoot {
		t.Errorf("container options %+v", o)
	}
	if c.DockerKillTimeout() != 30*time.Second {
		t.Errorf("kill timeout %s", c.DockerKillTimeout())
	}
	if got := c.EgressAllow(); len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Errorf("egress allow %v", got)
	}
	if len(Default().EgressAllow()) == 0 {
		t.Error("default egress allowlist is empty")
	}
	// user: "host" resolves to the harness ids on Linux and to the image user elsewhere.
	c.Worker.Docker.User = "host"
	if got, want := c.ContainerOptions().User, containerUserForHost(); got != want {
		t.Errorf("user %q want %q", got, want)
	}
}

// containerUserForHost mirrors container.UserForHost without importing it into
// the assertion, keeping the expectation platform-correct.
func containerUserForHost() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// TestShippedConfigsLoad keeps the deployment files honest. config.Load
// rejects unknown fields, so a chart (or a compose file) that renders a key
// the harness does not understand fails here rather than at rollout, when the
// pod crash-loops with a parse error nobody is watching for.
func TestShippedConfigsLoad(t *testing.T) {
	cases := []struct {
		path                            string
		driver, worker, session, dbFlag string
	}{
		{path: "../../deploy/harness.compose.yaml", driver: "sqlite", worker: "docker", session: "local"},
		{path: "../../deploy/harness.k8s.yaml", driver: "postgres", worker: "k8s", session: "s3"},
		{path: "../../harness.example.yaml", driver: "sqlite", worker: "local", session: "local"},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			if _, err := os.Stat(tc.path); err != nil {
				t.Skipf("%s is not present", tc.path)
			}
			// The Postgres DSN is a variable name in the file; supply it so
			// Validate sees a complete config.
			t.Setenv("HARNESS_POSTGRES_DSN", "postgres://harness@localhost:5432/harness?sslmode=disable")
			c, err := Load(tc.path)
			if err != nil {
				t.Fatalf("%s does not load: %v", tc.path, err)
			}
			driver := c.Database.Driver
			if driver == "" {
				driver = "sqlite"
			}
			sess := c.Session.Store
			if sess == "" {
				sess = "local"
			}
			if driver != tc.driver || c.Worker.Mode != tc.worker || sess != tc.session {
				t.Errorf("driver=%s worker=%s session=%s, want %s/%s/%s", driver, c.Worker.Mode, sess, tc.driver, tc.worker, tc.session)
			}
		})
	}
}

// A relative -config path used to leave data_root relative (filepath.Dir of
// "harness.yaml" is "."), which resolves against the worker's cwd — the
// checkout — rather than the harness's directory.
func TestLoadRelativeConfigPathYieldsAbsoluteDataRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "harness.yaml"), []byte("data_root: .harness\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	c, err := Load("harness.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.DataRoot) {
		t.Fatalf("DataRoot %q is not absolute", c.DataRoot)
	}
	for name, got := range map[string]string{
		"WorkspaceRoot": c.WorkspaceRoot(),
		"ConfigDirRoot": c.ConfigDirRoot(),
		"SessionRoot":   c.SessionRoot(),
		"AuditRoot":     c.AuditRoot(),
		"DBPath":        c.DBPath(),
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q, want absolute", name, got)
		}
	}
}

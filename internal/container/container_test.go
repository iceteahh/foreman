package container

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestArgsGolden(t *testing.T) {
	o := Options{Image: "harness/worker:cli-2.1.243", Network: "harness-internal", User: "1000:1000", EgressProxy: "http://egress:3128",
		Memory: "4g", CPUs: 2, PidsLimit: 512, ReadOnlyRoot: true, Labels: map[string]string{"harness.run_id": "run_1", "harness.kind": "code_fix"},
		ExtraArgs: []string{"--add-host=host.docker.internal:host-gateway"}}
	r := Run{Name: Name("run_01HXYZ", "worker"), Workspace: "/tmp/ws", ConfigDir: "/tmp/cfg", Env: []string{"ANTHROPIC_API_KEY", "GOFLAGS"}, Stdin: true,
		Cmd: []string{"claude", "-p", "do the thing; rm -rf /", "--session-id", "abc"}}
	got, err := Args(o, r)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := filepath.Abs("/tmp/ws")
	cfg, _ := filepath.Abs("/tmp/cfg")
	want := []string{"docker", "run", "--rm", "--init", "--name", "harness-run_01HXYZ-worker", "-i",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--network", "harness-internal", "--user", "1000:1000",
		"--memory", "4g", "--cpus", "2", "--pids-limit", "512",
		"--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=1g", "--tmpfs", "/home/worker:rw,exec,nosuid,size=1g",
		"-v", ws + ":/workspace", "-w", "/workspace", "-v", cfg + ":/run/claude-config", "-e", "CLAUDE_CONFIG_DIR=/run/claude-config",
		"-e", "HOME=/home/worker",
		"-e", "HTTP_PROXY=http://egress:3128", "-e", "HTTPS_PROXY=http://egress:3128", "-e", "http_proxy=http://egress:3128", "-e", "https_proxy=http://egress:3128",
		"-e", "NO_PROXY=localhost,127.0.0.1", "-e", "no_proxy=localhost,127.0.0.1",
		"-e", "ANTHROPIC_API_KEY", "-e", "GOFLAGS",
		"--label", "harness.kind=code_fix", "--label", "harness.run_id=run_1",
		"--add-host=host.docker.internal:host-gateway",
		"harness/worker:cli-2.1.243", "claude", "-p", "do the thing; rm -rf /", "--session-id", "abc"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv mismatch:\n got %q\nwant %q", got, want)
	}
	// The prompt stays one element: no shell ever sees it.
	if got[len(got)-3] != "do the thing; rm -rf /" {
		t.Errorf("prompt split: %q", got[len(got)-3])
	}
	// Secret values never appear in argv.
	for _, a := range got {
		if strings.HasPrefix(a, "ANTHROPIC_API_KEY=") {
			t.Errorf("secret value in argv: %s", a)
		}
	}
}

func TestArgsMinimalAndErrors(t *testing.T) {
	got, err := Args(Options{Image: "img"}, Run{Workspace: "/w", Cmd: []string{"sh", "-c", "true"}})
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(got, " ")
	for _, bad := range []string{"--name", "-i ", "--network", "--user", "--memory", "--cpus", "--pids-limit", "--read-only", "HTTPS_PROXY", "CLAUDE_CONFIG_DIR"} {
		if strings.Contains(s, bad) {
			t.Errorf("unexpected %q in %s", bad, s)
		}
	}
	if !strings.HasSuffix(s, "img sh -c true") {
		t.Errorf("tail: %s", s)
	}
	cases := []struct {
		o Options
		r Run
	}{
		{Options{}, Run{Workspace: "/w", Cmd: []string{"x"}}},
		{Options{Image: "i"}, Run{Cmd: []string{"x"}}},
		{Options{Image: "i"}, Run{Workspace: "/w"}},
		{Options{Image: "i", User: "a b"}, Run{Workspace: "/w", Cmd: []string{"x"}}},
		{Options{Image: "i"}, Run{Workspace: "/w", Cmd: []string{"x"}, Env: []string{"KEY=value"}}},
		{Options{Image: "i", ExtraArgs: []string{"a\nb"}}, Run{Workspace: "/w", Cmd: []string{"x"}}},
	}
	for i, c := range cases {
		if _, err := Args(c.o, c.r); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestNameKillVersionSplit(t *testing.T) {
	if n := Name("run_01HX/Y Z", "judge-1"); n != "harness-run_01HX-Y-Z-judge-1" {
		t.Errorf("name %q", n)
	}
	if strings.Join(KillArgs(Options{Docker: "/usr/bin/docker"}, "c1", "TERM"), " ") != "/usr/bin/docker kill --signal TERM c1" {
		t.Error("kill args")
	}
	if strings.Join(KillArgs(Options{}, "c1", ""), " ") != "docker kill c1" {
		t.Error("kill args without signal")
	}
	if strings.Join(VersionArgs(Options{Image: "img"}), " ") != "docker run --rm --network none --entrypoint claude img --version" {
		t.Error("version args")
	}
	names, env := SplitEnv([]string{"A=1", "B=x=y", "bad", "=v"})
	if strings.Join(names, ",") != "A,B" || strings.Join(env, ",") != "A=1,B=x=y" {
		t.Errorf("split %v %v", names, env)
	}
}

// TestHostPathMappingForSiblingContainers covers the case where the harness
// itself runs in a container: the daemon resolves every bind-mount source on
// the host, so the harness's own view of its data root must be rewritten.
func TestHostPathMappingForSiblingContainers(t *testing.T) {
	o := Options{Image: "img", HostPath: PathMap{From: "/var/lib/harness", To: "/Users/me/stack/data"}}
	got, err := Args(o, Run{Workspace: "/var/lib/harness/workspaces/run_1", ConfigDir: "/var/lib/harness/claude-config/run_1",
		Cmd: []string{"claude", "-p", "go"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-v /Users/me/stack/data/workspaces/run_1:/workspace") {
		t.Errorf("workspace source not rewritten to the host path:\n%s", joined)
	}
	if !strings.Contains(joined, "-v /Users/me/stack/data/claude-config/run_1:/run/claude-config") {
		t.Errorf("config dir source not rewritten:\n%s", joined)
	}
	// Paths outside the mapped prefix are untouched.
	got, _ = Args(o, Run{Workspace: "/tmp/elsewhere", Cmd: []string{"sh"}})
	if !strings.Contains(strings.Join(got, " "), "-v /tmp/elsewhere:/workspace") {
		t.Errorf("unrelated path rewritten: %v", got)
	}
	// The prefix must match a whole directory, not a string prefix.
	m := PathMap{From: "/var/lib/harness", To: "/host"}
	if got := m.rewrite("/var/lib/harness-other/x"); got != "/var/lib/harness-other/x" {
		t.Errorf("partial directory name rewritten: %s", got)
	}
	if got := m.rewrite("/var/lib/harness"); got != "/host" {
		t.Errorf("exact root not rewritten: %s", got)
	}
	// Without the harness in a container there is no rewrite at all.
	if got := (PathMap{}).rewrite("/a/b"); got != "/a/b" {
		t.Errorf("empty mapping rewrote %s", got)
	}
	// A half-configured mapping is a config error, not a silent no-op.
	for _, bad := range []PathMap{{From: "/a"}, {To: "/b"}, {From: "a", To: "/b"}, {From: "/a", To: "b"}} {
		if err := (Options{Image: "i", HostPath: bad}).Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

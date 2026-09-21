package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/container"
	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/task"
)

// fakeDocker stands in for the docker CLI. Invocations are recorded under
// $REC keyed by container name (argv.<name>, env.<name>, imported.<name>,
// kill.<name>.<signal>) and "run" executes the container command on the host
// in the bind-mounted workspace, with CLAUDE_CONFIG_DIR pointed back at the
// host directory. A "kill" for a container that has not been created yet
// fails exactly as the real client does.
func fakeDocker(t *testing.T) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	if err := os.MkdirAll(rec, 0o750); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
# Fake docker CLI. Records each invocation under $REC keyed by container name
# (never by a shared counter: invocations overlap), and for "run" executes the
# container command on the host inside the bind-mounted workspace. "kill"
# fails like the real client when the container does not exist yet, which is
# what makes the retry loop in container.Stop observable.
REC="` + rec + `"
case "$1" in
  kill)
    for last in "$@"; do :; done
    sig=KILL; prev=""
    for a in "$@"; do [ "$prev" = "--signal" ] && sig="$a"; prev="$a"; done
    printf '%s\n' "$@" > "$REC/kill.$last.$sig"
    if [ -f "$REC/pid.$last" ]; then
      p=$(cat "$REC/pid.$last")
      # Signal the container command's whole process group, the way the real
      # runner does. Two reasons it cannot be "kill -9 $p": a test that tells
      # TERM from KILL needs TERM to really be TERM, and $p is the shell
      # running the fixture, not the sleep doing the waiting. Signalling the
      # shell alone leaves that sleep holding the stdout pipe open, so the
      # harness reads for 30s instead of meeting its deadline; signalling the
      # children first instead races the shell spawning its next one.
      # The group first (setsid gave the command its own), then the bare pid
      # as the fallback for a platform without setsid.
      kill -"$sig" "-$p" 2>/dev/null || kill -"$sig" "$p" 2>/dev/null
      exit 0
    fi
    echo "Error response from daemon: No such container: $last" >&2
    exit 1
    ;;
  run) ;;
  *) echo "fake docker: unsupported $1" >&2; exit 125;;
esac
name=""; prev=""
for a in "$@"; do [ "$prev" = "--name" ] && name="$a"; prev="$a"; done
[ -z "$name" ] && name=anon
printf '%s\n' "$@" > "$REC/argv.$name"
env | sort > "$REC/env.$name"
shift
cfg=""; ws=""; entry=""; envnames=""
while [ $# -gt 0 ]; do
  case "$1" in
    --name|--network|--user|--memory|--cpus|--pids-limit|--tmpfs|--label|--cap-drop|--security-opt|-w) shift;;
    --entrypoint) entry="$2"; shift;;
    -v) case "$2" in *:/run/claude-config) cfg="${2%%:*}";; *:/workspace) ws="${2%%:*}";; esac; shift;;
    -e) case "$2" in *=*) ;; *) envnames="$envnames $2";; esac; shift;;
    --rm|--init|-i|--read-only) ;;
    *) break;;
  esac
  shift
done
shift   # the image
for n in $envnames; do eval "v=\${$n:-__MISSING__}"; echo "$n=$v" >> "$REC/imported.$name"; done
[ -n "$cfg" ] && export CLAUDE_CONFIG_DIR="$cfg"
[ -n "$ws" ] && cd "$ws"
cmd="$entry"
if [ -z "$cmd" ]; then cmd="$1"; shift; fi
[ "$cmd" = claude ] && cmd="${FAKE_CLAUDE:-claude}"
# Run the container command in its own process group so a kill can take the
# whole tree by negative pid, the way the real runner does. It matters because
# dash does not exec the last command of a multi-command script the way bash
# does: the recorded pid is then the fixture's shell and its sleep survives,
# holding the stdout pipe open long after the deadline. (set -m cannot do this
# here - a test process has no controlling tty, so job control stays off.)
if command -v setsid >/dev/null 2>&1; then setsid "$cmd" "$@" & else "$cmd" "$@" & fi
child=$!
echo "$child" > "$REC/pid.$name"
wait $child
code=$?
rm -f "$REC/pid.$name"
exit $code`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

func dockerRunner(t *testing.T, claudeBody string) (*Runner, string, string) {
	t.Helper()
	claude, _ := fakeClaude(t, claudeBody)
	docker, rec := fakeDocker(t)
	t.Setenv("FAKE_CLAUDE", claude)
	rn, root := newRunner(t, "")
	rn.Launcher = &DockerLauncher{
		Options:   container.Options{Docker: docker, Image: "harness/worker:test", Network: "harness-internal", EgressProxy: "http://egress:3128", User: "1000:1000"},
		ClientEnv: []string{"PATH", "HOME", "FAKE_CLAUDE"},
	}
	return rn, root, rec
}

func TestDockerLauncherRunsInContainerWithEnvByName(t *testing.T) {
	rn, root, rec := dockerRunner(t, `cat "`+fixturePath("stream_sigkill_mid_tool_use.ndjson")+`"; cat "`+fixturePath("result_success.json")+`"; exit 0`)
	tk, r := baseTask(), run(task.SessionNew)
	res, err := rn.Run(context.Background(), tk, r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != events.OutcomeCompleted || res.ExitCode != 0 || res.Result == nil || res.Events != 7 {
		t.Errorf("result %+v", res)
	}
	if res.SessionURI == "" {
		t.Error("transcript written into the bind-mounted config dir was not snapshotted")
	}
	cname := "harness-" + r.ID
	argv, err := os.ReadFile(filepath.Join(rec, "argv."+cname))
	if err != nil {
		t.Fatalf("run not recorded: %v", err)
	}
	a := string(argv)
	for _, want := range []string{"run\n--rm\n--init\n--name\nharness-" + r.ID + "\n", "--network\nharness-internal\n", "--user\n1000:1000\n",
		"-v\n" + filepath.Join(root, "ws") + ":/workspace\n", "-v\n" + filepath.Join(root, "cfg") + ":/run/claude-config\n",
		"-e\nCLAUDE_CONFIG_DIR=/run/claude-config\n", "-e\nHTTPS_PROXY=http://egress:3128\n", "-e\nANTHROPIC_API_KEY\n", "-e\nGOFLAGS\n",
		"harness/worker:test\nclaude\n-p\n" + tk.Prompt + "\n"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv missing %q:\n%s", want, a)
		}
	}
	// Secret values never appear in argv, but the client env carries them for `-e NAME`.
	if strings.Contains(a, "sk-test") {
		t.Error("API key leaked into argv")
	}
	if strings.Contains(a, "-e\nPATH") || strings.Contains(a, "-e\nHOME\n") || strings.Contains(a, "-e\nCLAUDE_CONFIG_DIR\n") {
		t.Errorf("host-only variable imported into the container:\n%s", a)
	}
	imported, _ := os.ReadFile(filepath.Join(rec, "imported."+cname))
	if !strings.Contains(string(imported), "ANTHROPIC_API_KEY=sk-test") || !strings.Contains(string(imported), "GOFLAGS=-mod=mod") {
		t.Errorf("client env did not carry the imported variables: %s", imported)
	}
	if strings.Contains(string(imported), "MISSING") {
		t.Errorf("imported variable absent from client env: %s", imported)
	}
	// Stdin is not attached unless the prompt goes through it.
	if strings.Contains(a, "\n-i\n") {
		t.Error("-i without PromptViaStdin")
	}
}

func TestDockerLauncherTimeoutKillsByName(t *testing.T) {
	rn, root, rec := dockerRunner(t, `echo '{"type":"system","subtype":"init"}'; sleep 30`)
	rn.Grace = 100 * time.Millisecond
	tk, r := baseTask(), run(task.SessionNew)
	tk.Policy.TimeoutMS = 300
	start := time.Now()
	res, err := rn.Run(context.Background(), tk, r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillTimeout || res.Outcome != events.OutcomeCrash {
		t.Errorf("killed=%q outcome=%s", res.Killed, res.Outcome)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("kill did not take effect promptly")
	}
	// The container was signalled by name: TERM first, KILL after the grace period.
	cname := "harness-" + r.ID
	term, err := os.ReadFile(filepath.Join(rec, "kill."+cname+".TERM"))
	if err != nil {
		t.Fatalf("docker kill --signal TERM was not invoked: %v", err)
	}
	if want := "kill\n--signal\nTERM\n" + cname + "\n"; string(term) != want {
		t.Errorf("kill argv %q want %q", term, want)
	}
	// The container died on TERM, so the grace-period SIGKILL never had to fire.
	if _, err := os.Stat(filepath.Join(rec, "kill."+cname+".KILL")); err == nil {
		t.Error("SIGKILL sent even though the container stopped on TERM")
	}
}

// TestDockerLauncherKillRetriesUntilContainerExists covers the race that a
// single `docker kill` loses: the deadline fires before `docker run` has
// created the container, so the first kill reports "No such container" and
// the worker would run unbounded without the retry loop in container.Stop.
func TestDockerLauncherKillRetriesUntilContainerExists(t *testing.T) {
	// The fake CLI only creates its pid file after a delay, so the first kill
	// attempts find nothing.
	rn, root, rec := dockerRunner(t, `sleep 1; echo '{"type":"system","subtype":"init"}'; sleep 30`)
	rn.Grace = 5 * time.Second
	dl := rn.Launcher.(*DockerLauncher)
	dl.KillTimeout = 30 * time.Second
	tk, r := baseTask(), run(task.SessionNew)
	tk.Policy.TimeoutMS = 50 // fires while `docker run` is still starting
	start := time.Now()
	res, err := rn.Run(context.Background(), tk, r, spawn(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if res.Killed != KillTimeout {
		t.Errorf("killed=%q", res.Killed)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("retrying kill took %s: the worker outlived its deadline", d)
	}
	if _, err := os.ReadFile(filepath.Join(rec, "kill.harness-"+r.ID+".TERM")); err != nil {
		t.Errorf("kill not recorded: %v", err)
	}
}

func TestDockerLauncherVersionAndSignalMapping(t *testing.T) {
	docker, rec := fakeDocker(t)
	claude, _ := fakeClaude(t, `echo "2.1.243 (Claude Code)"`)
	t.Setenv("FAKE_CLAUDE", claude)
	l := &DockerLauncher{Options: container.Options{Docker: docker, Image: "img"}, ClientEnv: []string{"PATH", "HOME", "FAKE_CLAUDE"}}
	// fake docker treats --entrypoint claude … --version as running the fake CLI.
	v, err := l.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "2.1.243" {
		t.Errorf("version %q", v)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv.anon"))
	if !strings.Contains(string(argv), "--network\nnone\n--entrypoint\nclaude\nimg\n--version") {
		t.Errorf("version argv:\n%s", argv)
	}
	if syscall.SIGTERM.String() == "" {
		t.Fatal("unreachable")
	}
	if _, err := (&DockerLauncher{}).Version(context.Background()); err == nil {
		t.Error("missing image must fail validation")
	}
}

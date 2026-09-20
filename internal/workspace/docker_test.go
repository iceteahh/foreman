package workspace

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/container"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
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
      kill -9 "$(cat "$REC/pid.$last")" 2>/dev/null
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
"$cmd" "$@" &
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

var quietWS = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func TestDockerWorkspaceExecRunsInContainer(t *testing.T) {
	origin := NewBareRepo(t, map[string]string{"README.md": "hi\n", "run.sh": "#!/bin/sh\necho in-container $PWD; echo \"GOFLAGS=$GOFLAGS HOME_IS=$HOME\"; exit ${CODE:-0}\n"})
	local, err := NewLocal(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	docker, rec := fakeDocker(t)
	d, err := NewDocker(local, container.Options{Docker: docker, Image: "img", Network: "net", EgressProxy: "http://egress:3128"})
	if err != nil {
		t.Fatal(err)
	}
	d.ClientEnv = []string{"PATH", "HOME"}
	t.Setenv("GOFLAGS", "-mod=vendor")
	tk := &task.Task{ID: task.NewTaskID(), Workspace: task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"}}
	r := &task.Run{ID: task.NewRunID()}
	ws, err := d.Provision(context.Background(), tk, r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Destroy() }()

	res, err := ws.Exec(context.Background(), "sh", "run.sh")
	if err != nil {
		t.Fatalf("exec: %v %+v", err, res)
	}
	if !strings.Contains(res.Stdout, "in-container "+ws.Path()) {
		t.Errorf("stdout %q", res.Stdout)
	}
	cname := "harness-" + r.ID + "-exec-1"
	argv, err := os.ReadFile(filepath.Join(rec, "argv."+cname))
	if err != nil {
		t.Fatalf("exec not recorded: %v", err)
	}
	a := string(argv)
	for _, want := range []string{"run\n--rm\n--init\n--name\nharness-" + r.ID + "-exec-1\n", "--network\nnet\n", "-v\n" + ws.Path() + ":/workspace\n-w\n/workspace\n",
		"-e\nHTTPS_PROXY=http://egress:3128\n", "-e\nGOFLAGS\n", "img\nsh\nrun.sh\n"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv missing %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "CLAUDE_CONFIG_DIR") || strings.Contains(a, "GOFLAGS=") {
		t.Errorf("acceptance container must get no CLI state and no env values in argv:\n%s", a)
	}
	env, _ := os.ReadFile(filepath.Join(rec, "env."+cname))
	if !strings.Contains(string(env), "GOFLAGS=-mod=vendor") {
		t.Errorf("client env lacks imported var: %s", env)
	}

	// Non-zero exit is reported like the local workspace.
	t.Setenv("CODE", "3")
	d.Env = append(d.Env, "CODE")
	res, err = ws.Exec(context.Background(), "sh", "run.sh")
	if err == nil || res.ExitCode != 3 {
		t.Errorf("exit code %d err %v", res.ExitCode, err)
	}

	// Capture still works on the host checkout.
	if err := os.WriteFile(filepath.Join(ws.Path(), "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ws.Capture(context.Background())
	if err != nil || len(c.ChangedFiles) != 1 || c.ChangedFiles[0] != "new.txt" {
		t.Errorf("capture %+v %v", c, err)
	}
	if _, err := d.Open(context.Background(), tk, r); err != nil {
		t.Errorf("open: %v", err)
	}
}

func TestDockerWorkspaceExecTimeoutKillsContainer(t *testing.T) {
	origin := NewBareRepo(t, map[string]string{"README.md": "hi\n"})
	local, _ := NewLocal(filepath.Join(t.TempDir(), "ws"))
	docker, rec := fakeDocker(t)
	d, _ := NewDocker(local, container.Options{Docker: docker, Image: "img"})
	d.ClientEnv = []string{"PATH", "HOME"}
	tk := &task.Task{ID: task.NewTaskID(), Workspace: task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"}}
	r := &task.Run{ID: task.NewRunID()}
	ws, err := d.Provision(context.Background(), tk, r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Destroy() }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := ws.Exec(ctx, "sleep", "30")
	if err == nil || !res.TimedOut {
		t.Errorf("expected timeout, got %+v %v", res, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("timeout did not stop the command")
	}
	cname := "harness-" + r.ID + "-exec-1"
	argv, err := os.ReadFile(filepath.Join(rec, "kill."+cname+".KILL"))
	if err != nil {
		t.Fatalf("docker kill was not invoked: %v", err)
	}
	if want := "kill\n--signal\nKILL\n" + cname + "\n"; string(argv) != want {
		t.Errorf("kill argv %q want %q", argv, want)
	}
}

// TestDockerWorkspaceExecKillRetriesUntilContainerExists covers the same race
// as the runner: a deadline that fires before `docker run` created the
// container must not leave the acceptance command running.
func TestDockerWorkspaceExecKillRetriesUntilContainerExists(t *testing.T) {
	origin := NewBareRepo(t, map[string]string{"slow.sh": "#!/bin/sh\nsleep 1\nsleep 30\n"})
	local, _ := NewLocal(filepath.Join(t.TempDir(), "ws"))
	docker, rec := fakeDocker(t)
	d, _ := NewDocker(local, container.Options{Docker: docker, Image: "img"})
	d.ClientEnv = []string{"PATH", "HOME"}
	d.Logger = quietWS
	tk := &task.Task{ID: task.NewTaskID(), Workspace: task.WorkspaceSpec{Type: "git", Repo: origin, Ref: "main"}}
	r := &task.Run{ID: task.NewRunID()}
	ws, err := d.Provision(context.Background(), tk, r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Destroy() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, _ := ws.Exec(ctx, "sh", "slow.sh")
	if !res.TimedOut {
		t.Error("TimedOut not reported")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("exec ran %s past its deadline", d)
	}
	if _, err := os.ReadFile(filepath.Join(rec, "kill.harness-"+r.ID+"-exec-1.KILL")); err != nil {
		t.Errorf("kill not recorded: %v", err)
	}
}

func TestNewDockerValidates(t *testing.T) {
	if _, err := NewDocker(nil, container.Options{Image: "i"}); err == nil {
		t.Error("nil local accepted")
	}
	if _, err := NewDocker(&Local{}, container.Options{}); err == nil {
		t.Error("missing image accepted")
	}
}

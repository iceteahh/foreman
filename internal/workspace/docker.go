package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/container"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Docker is a Manager whose workspaces run acceptance commands inside the
// worker image (plan Step 14). Git plumbing (clone, diff, commit, push) stays
// on the host through the embedded Local manager, so the repository token is
// never visible from inside a container; only Exec crosses into the sandbox,
// on the same network and with the same limits as the worker itself.
type Docker struct {
	Local   *Local
	Options container.Options
	// Env lists variable names exported into the acceptance container from
	// the harness environment (toolchain flags, never host paths). Default
	// DefaultDockerEnv plus harness.yaml `env:` keys.
	Env []string
	// ClientEnv is what the docker client needs (default runner.DefaultClientEnv equivalent).
	ClientEnv []string
	// KillTimeout bounds the stop loop after a context deadline (default 2m).
	KillTimeout time.Duration
	Logger      *slog.Logger
}

func (d *Docker) log() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// DefaultDockerEnv is imported into acceptance containers.
var DefaultDockerEnv = []string{"LANG", "LC_ALL", "GOFLAGS", "GOPROXY", "GONOSUMDB", "GOPRIVATE", "NODE_OPTIONS", "npm_config_registry"}

var dockerClientEnv = []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}

// NewDocker wraps a Local manager.
func NewDocker(local *Local, opts container.Options) (*Docker, error) {
	if local == nil {
		return nil, errors.New("workspace: docker manager needs a local manager for git")
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return &Docker{Local: local, Options: opts, Env: append([]string(nil), DefaultDockerEnv...)}, nil
}

// Provision implements Manager.
func (d *Docker) Provision(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error) {
	ws, err := d.Local.Provision(ctx, t, r)
	if err != nil {
		return nil, err
	}
	return &dockerWorkspace{Workspace: ws, d: d, runID: r.ID}, nil
}

// Open implements Manager.
func (d *Docker) Open(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error) {
	ws, err := d.Local.Open(ctx, t, r)
	if err != nil {
		return nil, err
	}
	return &dockerWorkspace{Workspace: ws, d: d, runID: r.ID}, nil
}

// dockerWorkspace runs Exec in a throwaway container; everything else is the host checkout.
type dockerWorkspace struct {
	Workspace
	d     *Docker
	runID string
	seq   int
}

// Exec runs argv inside a fresh container sharing the checkout. The container
// is killed by name when ctx expires (CommandContext alone would only signal
// the docker client).
func (w *dockerWorkspace) Exec(ctx context.Context, name string, args ...string) (ExecResult, error) {
	w.seq++
	cname := container.Name(w.runID, fmt.Sprintf("exec-%d", w.seq))
	var names, kv []string
	for _, k := range w.d.Env {
		if v, ok := os.LookupEnv(k); ok {
			names = append(names, k)
			kv = append(kv, k+"="+v)
		}
	}
	argv, err := container.Args(w.d.Options, container.Run{Name: cname, Workspace: w.Path(), Env: names, Cmd: append([]string{name}, args...)})
	if err != nil {
		return ExecResult{Cmd: append([]string{name}, args...)}, err
	}
	start := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // killed by name below
	cmd.Env = w.d.clientEnv(kv)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	res := ExecResult{Cmd: append([]string{name}, args...)}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("docker run: %w", err)
	}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(exited)
		done <- err
	}()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		res.TimedOut = true
		w.kill(cname, exited)
		waitErr = <-done
	}
	res.Stdout, res.Stderr, res.Duration = out.String(), errb.String(), time.Since(start)
	var ee *exec.ExitError
	switch {
	case waitErr == nil:
		res.ExitCode = 0
	case errors.As(waitErr, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == -1 {
			res.ExitCode = 137
		}
	default:
		return res, waitErr
	}
	if res.ExitCode == 125 || res.ExitCode == 126 || res.ExitCode == 127 {
		// docker itself failed (bad image, missing binary) — surface it clearly.
		return res, fmt.Errorf("%s exited %d (docker: %s)", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s exited %d", name, res.ExitCode)
	}
	return res, nil
}

// kill stops the acceptance container. It retries until docker confirms the
// container existed or the client exits: `docker run` creates the container
// asynchronously, so a deadline firing right after start would otherwise kill
// nothing and leave the command running to completion.
func (w *dockerWorkspace) kill(cname string, exited <-chan struct{}) {
	if err := container.Stop(w.d.Options, cname, "KILL", w.d.clientEnv(nil), exited,
		container.StopOptions{Timeout: w.d.KillTimeout}); err != nil {
		w.d.log().Warn("stopping acceptance container failed", "container", cname, "err", err)
	}
}

func (d *Docker) clientEnv(extra []string) []string {
	keys := d.ClientEnv
	if keys == nil {
		keys = dockerClientEnv
	}
	env := make([]string, 0, len(keys)+len(extra))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

var (
	_ Manager   = (*Docker)(nil)
	_ Workspace = (*dockerWorkspace)(nil)
)

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/container"
)

// DockerLauncher runs `claude -p` inside the pinned worker image: one
// container per run, the workspace and the per-run CLAUDE_CONFIG_DIR
// bind-mounted, credentials imported from the docker client's environment
// (never in argv or on disk), egress through the allowlisting proxy
// (design §4.1, plan Step 14).
type DockerLauncher struct {
	Options container.Options
	// ClientEnv lists host variables the docker client itself needs
	// (default: PATH, HOME, DOCKER_HOST, DOCKER_CONFIG, DOCKER_CONTEXT,
	// DOCKER_TLS_VERIFY, DOCKER_CERT_PATH). They are not imported into the container.
	ClientEnv []string
	// KillTimeout bounds the stop loop after a kill request (default 2m).
	KillTimeout time.Duration
}

// DefaultClientEnv is what the docker CLI needs from the host.
var DefaultClientEnv = []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"}

// hostOnlyEnv never crosses into the container: they carry host paths or are
// set by the container builder itself.
var hostOnlyEnv = map[string]bool{"PATH": true, "HOME": true, "CLAUDE_CONFIG_DIR": true}

// Name implements Launcher.
func (*DockerLauncher) Name() string { return "docker" }

// Version implements Launcher: the CLI version baked into the image.
func (l *DockerLauncher) Version(ctx context.Context) (string, error) {
	if err := l.Options.Validate(); err != nil {
		return "", err
	}
	argv := container.VersionArgs(l.Options)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = l.clientEnv(nil)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("docker run %s claude --version: %w: %s", l.Options.Image, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("docker run %s claude --version: %w", l.Options.Image, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", errors.New("claude --version printed nothing inside the image")
	}
	return fields[0], nil
}

// Start implements Launcher.
func (l *DockerLauncher) Start(_ context.Context, sp Spawn, id string, args, env []string, stdin io.Reader) (Process, error) {
	var names, kv []string
	for _, pair := range env {
		k, _, ok := strings.Cut(pair, "=")
		if !ok || hostOnlyEnv[k] {
			continue
		}
		names = append(names, k)
		kv = append(kv, pair)
	}
	name := container.Name(id, "")
	argv, err := container.Args(l.Options, container.Run{
		Name: name, Workspace: sp.Workspace, ConfigDir: sp.ConfigDir, Env: names, Stdin: stdin != nil,
		Cmd: append([]string{"claude"}, args...),
	})
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // the runner kills the container by name; CommandContext would only signal the docker client
	cmd.Env = l.clientEnv(kv)
	cmd.Stdin = stdin
	// The client gets its own process group too, so a SIGKILL fallback after
	// `docker kill` also reaps a hung client.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startCmd(cmd, func(p *cmdProcess, sig syscall.Signal) error { return l.kill(p, name, sig) }, func() string { return name })
}

// kill forwards the signal to the container, retrying until docker confirms
// the container existed or the client exits: `docker run` creates the
// container asynchronously, so a timeout that fires right after start would
// otherwise signal nothing and leave the worker running. On SIGKILL the docker
// client's own process group is killed too, so a wedged client cannot hold the
// runner open after the container is gone.
func (l *DockerLauncher) kill(p *cmdProcess, name string, sig syscall.Signal) error {
	signal := "TERM"
	if sig == syscall.SIGKILL {
		signal = "KILL"
	}
	err := container.Stop(l.Options, name, signal, l.clientEnv(nil), p.exited, container.StopOptions{Timeout: l.KillTimeout})
	if sig == syscall.SIGKILL && p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	return err
}

func (l *DockerLauncher) clientEnv(extra []string) []string {
	keys := l.ClientEnv
	if keys == nil {
		keys = DefaultClientEnv
	}
	env := make([]string, 0, len(keys)+len(extra))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

var _ Launcher = (*DockerLauncher)(nil)

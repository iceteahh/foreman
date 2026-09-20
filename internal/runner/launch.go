package runner

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// Process is a started worker the runner supervises. Local processes and
// containers both implement it; the runner's supervision loop (decode stdout,
// timeout, budget backstop, kill) is identical for both.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait blocks until the worker exits. exitCode is the process status
	// (128+signal when signalled, -1 when unknown).
	Wait() (exitCode int, signaled bool, err error)
	// Signal delivers sig to everything the worker started: the whole process
	// group locally, the container (and its init) under docker.
	Signal(sig syscall.Signal) error
	// ID identifies the worker in logs (pid or container name).
	ID() string
}

// Launcher turns the CLI argv into a running Process. LocalLauncher execs the
// installed CLI; DockerLauncher runs it inside the pinned worker image
// (plan Step 14). The runner owns everything else.
type Launcher interface {
	Name() string
	// Start launches `claude <args>` with env (KEY=VALUE, already allowlisted)
	// in the workspace. id is the run id (container naming). stdin may be nil.
	Start(ctx context.Context, sp Spawn, id string, args, env []string, stdin io.Reader) (Process, error)
	// Version reports the CLI version the launcher would run.
	Version(ctx context.Context) (string, error)
}

// LocalLauncher execs the CLI on the host in its own process group.
type LocalLauncher struct {
	// Bin is the executable (default DefaultBin).
	Bin string
}

func (l *LocalLauncher) bin() string {
	if l.Bin == "" {
		return DefaultBin
	}
	return l.Bin
}

// Name implements Launcher.
func (*LocalLauncher) Name() string { return "local" }

// Version implements Launcher.
func (l *LocalLauncher) Version(ctx context.Context) (string, error) { return CLIVersion(ctx, l.bin()) }

// Start implements Launcher.
func (l *LocalLauncher) Start(_ context.Context, sp Spawn, _ string, args, env []string, stdin io.Reader) (Process, error) {
	cmd := exec.Command(l.bin(), args...) //nolint:noctx // ctx is honoured by the runner killing the whole process group; CommandContext would only signal the leader pid
	cmd.Dir = sp.Workspace
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = stdin
	return startCmd(cmd, func(_ *cmdProcess, sig syscall.Signal) error { return syscall.Kill(-cmd.Process.Pid, sig) },
		func() string { return strconv.Itoa(cmd.Process.Pid) })
}

// cmdProcess adapts an exec.Cmd to Process with a pluggable Signal. exited is
// closed when Wait returns, so a signal implementation that retries (docker)
// knows when to give up.
type cmdProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	signal func(*cmdProcess, syscall.Signal) error
	id     func() string
	exited chan struct{}
	once   sync.Once
}

func startCmd(cmd *exec.Cmd, signal func(*cmdProcess, syscall.Signal) error, id func() string) (Process, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdProcess{cmd: cmd, stdout: stdout, stderr: stderr, signal: signal, id: id, exited: make(chan struct{})}, nil
}

func (p *cmdProcess) Stdout() io.Reader               { return p.stdout }
func (p *cmdProcess) Stderr() io.Reader               { return p.stderr }
func (p *cmdProcess) Signal(sig syscall.Signal) error { return p.signal(p, sig) }
func (p *cmdProcess) ID() string                      { return p.id() }

func (p *cmdProcess) Wait() (int, bool, error) {
	err := p.cmd.Wait()
	p.once.Do(func() { close(p.exited) })
	return exitStatus(err)
}

// exitStatus classifies exec.Cmd.Wait's error into (exit code, signalled).
func exitStatus(err error) (int, bool, error) {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, false, nil
	case errors.As(err, &ee):
		code := ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), true, nil
		}
		return code, false, nil
	default:
		return -1, false, err
	}
}

var _ Launcher = (*LocalLauncher)(nil)

package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/k8s"
)

// K8sLauncher runs `claude -p` as a Kubernetes Job: one Job per run, on the
// worker node pool, with the checkout on a shared volume and the credential
// coming from a Secret (plan Step 22, design §10).
//
// The supervision contract is the runner's, unchanged: the launcher hands back
// a Process with stdout, stderr, a signal and an exit code. What differs is
// where they come from — stdout is `kubectl logs --follow`, the signal is a
// Job deletion, and the exit code is read from the pod's terminated state
// after the log stream ends.
type K8sLauncher struct {
	Options k8s.Options
	// ClientEnv lists host variables kubectl itself needs (default:
	// PATH, HOME, KUBECONFIG and the in-cluster service-account variables).
	// The worker's own environment comes from the manifest, never from here.
	ClientEnv []string
	// StartTimeout bounds how long `kubectl logs` waits for the pod to be
	// scheduled and pulled (default 5 minutes).
	StartTimeout time.Duration
	// StatusTimeout bounds the wait for the pod's terminated state after the
	// log stream ends (default 2 minutes).
	StatusTimeout time.Duration
	// StatusPoll is the interval between status reads (default 2 seconds).
	StatusPoll time.Duration
	// Grace is the deletion grace period on SIGTERM (default 30 seconds).
	Grace time.Duration
}

// DefaultK8sClientEnv is what kubectl needs from the harness's environment.
var DefaultK8sClientEnv = []string{
	"PATH", "HOME", "KUBECONFIG",
	"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT",
}

// Name implements Launcher.
func (*K8sLauncher) Name() string { return "k8s" }

// Version implements Launcher: the CLI version inside the image, checked by
// running it in the cluster, so a node pool pinned to a stale image is caught
// at startup rather than by a golden case a week later.
func (l *K8sLauncher) Version(ctx context.Context) (string, error) {
	if err := l.Options.Validate(); err != nil {
		return "", err
	}
	argv := k8s.VersionArgs(l.Options)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = l.clientEnv()
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("kubectl run %s claude --version: %w: %s", l.Options.Image, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("kubectl run %s claude --version: %w", l.Options.Image, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", errors.New("claude --version printed nothing inside the image")
	}
	return fields[0], nil
}

// Start implements Launcher. stdin is not supported: a Job's stdin would have
// to be attached before the pod is scheduled, so worker.prompt_via_stdin is
// rejected in k8s mode rather than silently dropping the prompt.
func (l *K8sLauncher) Start(ctx context.Context, sp Spawn, id string, args, env []string, stdin io.Reader) (Process, error) {
	if stdin != nil {
		return nil, errors.New("k8s launcher: worker.prompt_via_stdin is not supported (the prompt travels in argv)")
	}
	name := k8s.Name(id, "")
	manifest, err := k8s.Manifest(l.Options, k8s.Run{
		Name: name, RunID: id, Workspace: sp.Workspace, ConfigDir: sp.ConfigDir,
		Env: nonSecretEnv(env), Cmd: append([]string{"claude"}, args...),
	})
	if err != nil {
		return nil, err
	}
	create := k8s.CreateArgs(l.Options)
	cmd := exec.CommandContext(ctx, create[0], create[1:]...)
	cmd.Env = l.clientEnv()
	cmd.Stdin = bytes.NewReader(manifest)
	var createErr bytes.Buffer
	cmd.Stderr = &createErr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl create job %s: %w: %s", name, err, strings.TrimSpace(createErr.String()))
	}

	// Follow the pod's output. A Job's container has one output stream, so the
	// worker's stderr and its NDJSON events arrive interleaved; demux splits
	// them back apart by whether a line parses as JSON, which is what keeps
	// the runner's stderr classification (session lost, id in use) working.
	logs := k8s.LogsArgs(l.Options, name, l.startTimeout())
	logCmd := exec.Command(logs[0], logs[1:]...) //nolint:noctx // the runner deletes the Job on cancellation; killing the kubectl client alone would leave the worker running
	logCmd.Env = l.clientEnv()
	logCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stream, err := logCmd.StdoutPipe()
	if err != nil {
		l.deleteJob(name, 0)
		return nil, err
	}
	// kubectl's own diagnostics ("waiting for pod …") are not worker output.
	var clientErr bytes.Buffer
	logCmd.Stderr = &clientErr
	if err := logCmd.Start(); err != nil {
		l.deleteJob(name, 0)
		return nil, err
	}
	p := &k8sProcess{
		launcher: l, name: name, cmd: logCmd, clientErr: &clientErr,
		outR: newPipe(), errR: newPipe(), exited: make(chan struct{}),
	}
	go p.demux(stream)
	return p, nil
}

// nonSecretEnv drops the variables that must not be written into a manifest:
// credentials come from the Secret, and PATH/HOME/CLAUDE_CONFIG_DIR describe
// the harness's own machine, not the pod's.
func nonSecretEnv(env []string) []string {
	skip := map[string]bool{
		"PATH": true, "HOME": true, "CLAUDE_CONFIG_DIR": true,
		"ANTHROPIC_API_KEY": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	}
	var out []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || skip[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (l *K8sLauncher) clientEnv() []string {
	keys := l.ClientEnv
	if keys == nil {
		keys = DefaultK8sClientEnv
	}
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (l *K8sLauncher) startTimeout() time.Duration {
	if l.StartTimeout > 0 {
		return l.StartTimeout
	}
	return 5 * time.Minute
}

func (l *K8sLauncher) statusTimeout() time.Duration {
	if l.StatusTimeout > 0 {
		return l.StatusTimeout
	}
	return 2 * time.Minute
}

func (l *K8sLauncher) statusPoll() time.Duration {
	if l.StatusPoll > 0 {
		return l.StatusPoll
	}
	return 2 * time.Second
}

func (l *K8sLauncher) grace() time.Duration {
	if l.Grace > 0 {
		return l.Grace
	}
	return 30 * time.Second
}

func (l *K8sLauncher) deleteJob(name string, grace time.Duration) {
	argv := k8s.DeleteArgs(l.Options, name, grace)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = l.clientEnv()
	_ = cmd.Run()
}

// k8sProcess adapts a Job plus its log stream to runner.Process.
type k8sProcess struct {
	launcher  *K8sLauncher
	name      string
	cmd       *exec.Cmd
	clientErr *bytes.Buffer

	outR, errR *pipe
	exited     chan struct{}
	once       sync.Once

	killMu  sync.Mutex
	deleted bool
}

func (p *k8sProcess) Stdout() io.Reader { return p.outR }
func (p *k8sProcess) Stderr() io.Reader { return p.errR }
func (p *k8sProcess) ID() string        { return p.name }

// demux splits the pod's single output stream: NDJSON lines are the worker's
// stdout, everything else is treated as its stderr.
func (p *k8sProcess) demux(stream io.ReadCloser) {
	defer func() {
		_ = stream.Close()
		p.outR.close()
		p.errR.close()
	}()
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64<<10), 10<<20) // a tool result can be large
	for sc.Scan() {
		line := sc.Bytes()
		t := bytes.TrimSpace(line)
		if len(t) > 0 && t[0] == '{' {
			p.outR.write(append(append([]byte(nil), line...), '\n'))
			continue
		}
		if len(t) > 0 {
			p.errR.write(append(append([]byte(nil), line...), '\n'))
		}
	}
	if err := sc.Err(); err != nil {
		p.errR.write([]byte("harness: reading pod logs: " + err.Error() + "\n"))
	}
}

// Signal implements Process. There is no signal to send to a pod from
// outside, so SIGTERM deletes the Job with a grace period — the kubelet then
// sends the container SIGTERM and SIGKILLs it when the grace expires — and
// SIGKILL deletes it with grace 0.
func (p *k8sProcess) Signal(sig syscall.Signal) error {
	grace := p.launcher.grace()
	if sig == syscall.SIGKILL {
		grace = 0
	}
	p.killMu.Lock()
	p.deleted = true
	p.killMu.Unlock()
	p.launcher.deleteJob(p.name, grace)
	if sig == syscall.SIGKILL && p.cmd.Process != nil {
		// The log follower can outlive the Job it was following.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

// Wait blocks until the log stream ends and the pod reports a terminated
// state. A Job that was deleted reports 137 (SIGKILL), matching what the
// runner would see from a killed local process group.
func (p *k8sProcess) Wait() (int, bool, error) {
	err := p.cmd.Wait()
	p.once.Do(func() { close(p.exited) })
	p.killMu.Lock()
	deleted := p.deleted
	p.killMu.Unlock()

	st, ok := p.waitForTermination()
	switch {
	case ok && st.Signal > 0:
		return 128 + st.Signal, true, nil
	case ok:
		return st.ExitCode, false, nil
	case deleted:
		// We deleted the Job, so there is nothing left to ask; report it the
		// way a SIGKILLed process group is reported.
		return 137, true, nil
	}
	// No terminated state and nobody killed it: the log follower itself failed
	// (RBAC, a pod that never started). Surface kubectl's own error.
	msg := strings.TrimSpace(p.clientErr.String())
	if err != nil {
		return -1, false, fmt.Errorf("kubectl logs job/%s: %w: %s", p.name, err, msg)
	}
	if msg != "" {
		return -1, false, fmt.Errorf("kubectl logs job/%s ended without a pod status: %s", p.name, msg)
	}
	return -1, false, fmt.Errorf("job %s ended without a pod status", p.name)
}

// waitForTermination polls the pod until its worker container reports a
// terminated state. The log stream can end a moment before the API has the
// status, so a short poll is normal, not a failure.
func (p *k8sProcess) waitForTermination() (k8s.TerminatedState, bool) {
	deadline := time.Now().Add(p.launcher.statusTimeout())
	argv := k8s.StatusArgs(p.launcher.Options, p.name)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = p.launcher.clientEnv()
		out, err := cmd.Output()
		cancel()
		if err == nil {
			if st, ok := k8s.ParseStatus(out); ok {
				return st, true
			}
		}
		if time.Now().After(deadline) {
			return k8s.TerminatedState{}, false
		}
		time.Sleep(p.launcher.statusPoll())
	}
}

// pipe is a minimal in-memory stream the demux writes and the runner reads.
// io.Pipe would deadlock here: the runner reads stdout to completion before it
// drains stderr, so a blocking stderr write would stall the whole demux.
type pipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newPipe() *pipe {
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *pipe) write(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.buf.Write(b)
	p.cond.Broadcast()
}

func (p *pipe) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

func (p *pipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 {
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}
	return p.buf.Read(b)
}

var _ Launcher = (*K8sLauncher)(nil)

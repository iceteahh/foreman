// Package container builds `docker run` argument vectors for the worker image
// (plan Step 14, design §4.1). It is a pure argv builder shared by the runner
// (the `claude -p` process) and the workspace (acceptance commands), so both
// run under the same image, network, user and resource limits. No shell is
// ever involved: every value is one argv element and secrets travel through
// the docker client's environment (`-e NAME` without a value), never argv.
package container

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Mount points inside the worker container. The workspace is bind-mounted at
// WorkspaceDir and the per-run CLAUDE_CONFIG_DIR at ConfigDir; both are host
// directories, so the runner snapshots the transcript straight from the host.
const (
	WorkspaceDir = "/workspace"
	ConfigDir    = "/run/claude-config"
	// HomeDir is the image user's home. It is set explicitly because
	// `--user <uid>` with no passwd entry leaves HOME at "/", which git and
	// npm cannot write to. This is the container's HOME, not the host's: the
	// "never override HOME" rule protects the host worker's git/ssh config.
	HomeDir = "/home/worker"
)

// Options select the image and its sandbox.
type Options struct {
	// Docker is the client executable (default "docker").
	Docker string
	// Image is the worker image (harness/worker:cli-<pinned>). Required.
	Image string
	// Network is the docker network the container joins. Use an `--internal`
	// bridge that only reaches the egress proxy (deploy/docker-compose.yaml);
	// "none" cuts all networking (useful for offline tests).
	Network string
	// User is passed as --user (uid[:gid]). Empty keeps the image user; on a
	// Linux host it should be the harness's own uid:gid so bind-mounted files
	// stay writable by both sides (UserForHost).
	User string
	// EgressProxy is exported as HTTP_PROXY/HTTPS_PROXY (and lower-case) so the
	// CLI, git, go and npm route through the allowlisting proxy.
	EgressProxy string
	// NoProxy is exported as NO_PROXY (default "localhost,127.0.0.1").
	NoProxy string
	// Resource limits; zero values are omitted.
	Memory    string  // --memory, e.g. "4g"
	CPUs      float64 // --cpus
	PidsLimit int     // --pids-limit
	// ReadOnlyRoot mounts the image filesystem read-only (tmpfs on /tmp and HOME).
	ReadOnlyRoot bool
	// ExtraArgs are appended verbatim before the image (operator escape hatch,
	// e.g. "--add-host=host.docker.internal:host-gateway").
	ExtraArgs []string
	// Labels are added as --label key=value (the runner adds run/task ids).
	Labels map[string]string
	// HostPath rewrites bind-mount sources from the harness's own view of a
	// path to the host's. It is required when the harness itself runs in a
	// container: worker containers are siblings started through the host
	// daemon, so the daemon resolves every -v source on the host, not inside
	// the harness container. Empty (the harness on the host) means no rewrite.
	HostPath PathMap
}

// PathMap rewrites a path prefix. From is the path as the harness sees it
// (its data root), To is the same directory on the host.
type PathMap struct {
	From, To string
}

// rewrite maps p into the host's namespace.
func (m PathMap) rewrite(p string) string {
	if m.From == "" || m.To == "" {
		return p
	}
	from := strings.TrimSuffix(m.From, "/")
	if p == from {
		return strings.TrimSuffix(m.To, "/")
	}
	if rest, ok := strings.CutPrefix(p, from+"/"); ok {
		return strings.TrimSuffix(m.To, "/") + "/" + rest
	}
	return p
}

// Valid reports whether the mapping is usable (both sides absolute, or unset).
func (m PathMap) Valid() error {
	if m.From == "" && m.To == "" {
		return nil
	}
	if m.From == "" || m.To == "" {
		return errors.New("container: host path mapping needs both sides")
	}
	if !filepath.IsAbs(m.From) || !filepath.IsAbs(m.To) {
		return fmt.Errorf("container: host path mapping %q -> %q must be absolute", m.From, m.To)
	}
	return nil
}

// Validate checks the options that every invocation needs.
func (o Options) Validate() error {
	var errs []error
	if strings.TrimSpace(o.Image) == "" {
		errs = append(errs, errors.New("container: image is required"))
	}
	if o.User != "" && !userRe.MatchString(o.User) {
		errs = append(errs, fmt.Errorf("container: user %q must be uid[:gid] or a name", o.User))
	}
	for _, a := range o.ExtraArgs {
		if strings.ContainsAny(a, "\n\r") {
			errs = append(errs, fmt.Errorf("container: extra arg %q contains a newline", a))
		}
	}
	if err := o.HostPath.Valid(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

var (
	userRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(:[A-Za-z0-9_.-]+)?$`)
	nameRe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// Name derives a valid container name from a run id and a suffix
// ("harness-run_01H…-worker"). Docker names allow [a-zA-Z0-9][a-zA-Z0-9_.-].
func Name(runID, suffix string) string {
	n := "harness-" + nameRe.ReplaceAllString(runID, "-")
	if suffix != "" {
		n += "-" + nameRe.ReplaceAllString(suffix, "-")
	}
	if len(n) > 128 {
		n = n[:128]
	}
	return n
}

// Run describes one container invocation.
type Run struct {
	// Name lets the runner `docker kill` the container when the client dies.
	Name string
	// Workspace is the host checkout mounted at WorkspaceDir (required).
	Workspace string
	// ConfigDir is the host per-run CLAUDE_CONFIG_DIR mounted at ConfigDir;
	// empty for acceptance commands, which need no CLI state.
	ConfigDir string
	// Env names the variables the container receives from the docker client's
	// environment (`-e NAME`). Values never appear in argv.
	Env []string
	// Stdin attaches stdin (-i) so the runner can pipe the prompt.
	Stdin bool
	// Cmd is the command inside the container (argv, no shell).
	Cmd []string
}

// Args builds the full `docker run …` argv (including "docker"). The
// container is removed on exit (--rm), runs with an init (--init) so tool
// children are reaped and signals reach them, drops every capability, and
// refuses privilege escalation. The workspace and config dir are the only
// writable bind mounts.
func Args(o Options, r Run) ([]string, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if r.Workspace == "" {
		return nil, errors.New("container: workspace is required")
	}
	if len(r.Cmd) == 0 {
		return nil, errors.New("container: command is required")
	}
	ws, err := filepath.Abs(r.Workspace)
	if err != nil {
		return nil, err
	}
	docker := o.Docker
	if docker == "" {
		docker = "docker"
	}
	args := []string{docker, "run", "--rm", "--init"}
	if r.Name != "" {
		args = append(args, "--name", r.Name)
	}
	if r.Stdin {
		args = append(args, "-i")
	}
	args = append(args, "--cap-drop", "ALL", "--security-opt", "no-new-privileges")
	if o.Network != "" {
		args = append(args, "--network", o.Network)
	}
	if o.User != "" {
		args = append(args, "--user", o.User)
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	if o.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(o.CPUs, 'f', -1, 64))
	}
	if o.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(o.PidsLimit))
	}
	if o.ReadOnlyRoot {
		args = append(args, "--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=1g", "--tmpfs", HomeDir+":rw,exec,nosuid,size=1g")
	}
	args = append(args, "-v", o.HostPath.rewrite(ws)+":"+WorkspaceDir, "-w", WorkspaceDir)
	if r.ConfigDir != "" {
		cd, err := filepath.Abs(r.ConfigDir)
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", o.HostPath.rewrite(cd)+":"+ConfigDir, "-e", "CLAUDE_CONFIG_DIR="+ConfigDir)
	}
	args = append(args, "-e", "HOME="+HomeDir)
	if o.EgressProxy != "" {
		noProxy := o.NoProxy
		if noProxy == "" {
			noProxy = "localhost,127.0.0.1"
		}
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			args = append(args, "-e", k+"="+o.EgressProxy)
		}
		args = append(args, "-e", "NO_PROXY="+noProxy, "-e", "no_proxy="+noProxy)
	}
	for _, name := range r.Env {
		if name == "" || strings.ContainsAny(name, "=\n") {
			return nil, fmt.Errorf("container: env entry %q must be a bare variable name", name)
		}
		args = append(args, "-e", name)
	}
	for _, k := range sortedKeys(o.Labels) {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	args = append(args, o.ExtraArgs...)
	args = append(args, o.Image)
	args = append(args, r.Cmd...)
	return args, nil
}

// KillArgs builds `docker kill --signal <sig> <name>`.
func KillArgs(o Options, name, signal string) []string {
	docker := o.Docker
	if docker == "" {
		docker = "docker"
	}
	args := []string{docker, "kill"}
	if signal != "" {
		args = append(args, "--signal", signal)
	}
	return append(args, name)
}

// VersionArgs builds the command that prints the CLI version baked into the image.
func VersionArgs(o Options) []string {
	docker := o.Docker
	if docker == "" {
		docker = "docker"
	}
	return []string{docker, "run", "--rm", "--network", "none", "--entrypoint", "claude", o.Image, "--version"}
}

// SplitEnv separates KEY=VALUE pairs into the names docker should import and
// the pairs to put in the client's environment.
func SplitEnv(pairs []string) (names []string, env []string) {
	for _, kv := range pairs {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		names = append(names, k)
		env = append(env, kv)
	}
	return names, env
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Kill asks docker to send signal to the named container. found is false when
// the container does not exist (yet) or has already stopped; that is not an
// error, because `docker run` creates the container asynchronously.
func Kill(ctx context.Context, o Options, name, signal string, env []string) (found bool, err error) {
	argv := KillArgs(o, name, signal)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	msg := strings.TrimSpace(string(out))
	if strings.Contains(msg, "No such container") || strings.Contains(msg, "is not running") || strings.Contains(msg, "No such object") {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, fmt.Errorf("docker kill %s: %w: %s", name, err, msg)
}

// StopOptions tune Stop.
type StopOptions struct {
	// Retry is the delay between attempts (default 200ms).
	Retry time.Duration
	// Timeout bounds the whole loop (default 2 minutes) and each docker call.
	Timeout time.Duration
}

// Stop signals the named container until docker confirms it existed, the
// worker has exited (stopped closed), or the timeout elapses.
//
// A single `docker kill` is not enough: the client creates the container
// asynchronously, so a deadline that fires just after start can arrive before
// the container exists, and the kill would be a silent no-op while the
// container keeps running. Retrying closes that window.
func Stop(o Options, name, signal string, env []string, stopped <-chan struct{}, so StopOptions) error {
	retry, timeout := so.Retry, so.Timeout
	if retry <= 0 {
		retry = 200 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		found, err := Kill(ctx, o, name, signal, env)
		cancel()
		if err != nil {
			lastErr = err
		}
		if found {
			return nil
		}
		select {
		case <-stopped:
			return lastErr // the worker is gone; nothing left to signal
		case <-time.After(retry):
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("docker kill %s: container never appeared within %s", name, timeout)
		}
	}
}

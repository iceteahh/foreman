// Package k8s builds the Kubernetes Job that runs one worker, and the kubectl
// argument vectors that create, follow, inspect and delete it (plan Step 22,
// design §10 "workers as Kubernetes Jobs, one Job per run").
//
// Like internal/container it is a pure builder: the manifest is marshalled
// from Go structs, never templated from strings, and no shell is ever
// involved. Credentials reach the worker through `envFrom: secretRef` — a
// reference to a Secret, not a value — so nothing sensitive appears in the
// manifest, in argv, or in the API server's audit log of what we submitted.
//
// It shells out to kubectl rather than linking client-go on purpose: the
// runner's supervision loop already speaks "process with stdout, stderr, a
// signal and an exit code", which is exactly what a kubectl subprocess is, and
// it keeps the harness free of a Kubernetes client dependency and its version
// treadmill.
package k8s

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Mount points inside the worker pod; they match the docker sandbox so a task
// behaves the same in both modes.
const (
	WorkspaceDir = "/workspace"
	ConfigDir    = "/run/claude-config"
	HomeDir      = "/home/worker"
)

// Options select the image and the pod's shape.
type Options struct {
	// Kubectl is the client executable (default "kubectl").
	Kubectl string
	// Kubeconfig is passed as --kubeconfig when set; empty uses the ambient
	// config (in-cluster service account, or the operator's ~/.kube/config).
	Kubeconfig string
	// Context is passed as --context when set.
	Context string
	// Namespace the Jobs are created in (default "default").
	Namespace string
	// Image is the worker image (harness/worker:cli-<pinned>). Required.
	Image string
	// ImagePullPolicy is the container's pull policy ("IfNotPresent" default).
	ImagePullPolicy string
	// CredentialSecret names a Secret whose keys become the worker's
	// environment (ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN). Required in
	// practice: it is the only way a credential reaches a worker Job.
	CredentialSecret string
	// ServiceAccount runs the pod; empty uses the namespace default. The
	// worker never talks to the API server, so the token is not mounted.
	ServiceAccount string
	// DataClaim is the ReadWriteMany PersistentVolumeClaim holding the data
	// root. The orchestrator writes the checkout and the empty
	// CLAUDE_CONFIG_DIR into it, and the worker Job mounts the matching
	// subPaths — which is why the claim must be RWX: the orchestrator pod and
	// the worker pod hold it at the same time, usually on different nodes.
	// Required. (Transcripts still go to object storage, so a *retry* does not
	// depend on this volume surviving; see session.S3.)
	DataClaim string
	// DataRoot is the data root as the orchestrator sees it. Mount subPaths
	// are computed by making the workspace and config dir relative to it, so
	// the worker mounts exactly the directories this run owns.
	DataRoot string
	// NodeSelector pins workers to their own node pool.
	NodeSelector map[string]string
	// Tolerations are rendered verbatim into the pod spec.
	Tolerations []map[string]any
	// CPURequest/CPULimit and MemoryRequest/MemoryLimit are Kubernetes
	// quantities ("500m", "4Gi"); empty fields are omitted.
	CPURequest, CPULimit       string
	MemoryRequest, MemoryLimit string
	// RunAsUser is the uid the container runs as (0 = the image's user).
	RunAsUser int64
	// FSGroup owns the mounted volume so the worker can write the checkout.
	FSGroup int64
	// ReadOnlyRoot mounts the image filesystem read-only with emptyDir on /tmp
	// and HOME.
	ReadOnlyRoot bool
	// EgressProxy is exported as HTTP(S)_PROXY, NoProxy as NO_PROXY.
	EgressProxy string
	NoProxy     string
	// TTLAfterFinished deletes a finished Job (default 10 minutes). The
	// harness has already streamed the logs into the audit sink by then.
	TTLAfterFinished time.Duration
	// Labels are added to the Job and pod on top of the harness's own.
	Labels map[string]string
	// Annotations are added to the pod (e.g. a mesh's sidecar opt-out).
	Annotations map[string]string
}

// Validate checks the options every invocation needs.
func (o Options) Validate() error {
	var errs []error
	if strings.TrimSpace(o.Image) == "" {
		errs = append(errs, errors.New("k8s: image is required"))
	}
	if strings.TrimSpace(o.DataClaim) == "" {
		errs = append(errs, errors.New("k8s: data_claim is required (the RWX volume the workspace lives on)"))
	}
	if strings.TrimSpace(o.DataRoot) == "" {
		errs = append(errs, errors.New("k8s: data_root is required to compute the worker's mount subPaths"))
	}
	if o.Namespace != "" && !nameRe.MatchString(o.Namespace) {
		errs = append(errs, fmt.Errorf("k8s: namespace %q is not a DNS-1123 label", o.Namespace))
	}
	return errors.Join(errs...)
}

func (o Options) kubectl() string {
	if o.Kubectl == "" {
		return "kubectl"
	}
	return o.Kubectl
}

func (o Options) namespace() string {
	if o.Namespace == "" {
		return "default"
	}
	return o.Namespace
}

// global returns the kubectl flags that apply to every subcommand.
func (o Options) global() []string {
	args := []string{"--namespace", o.namespace()}
	if o.Kubeconfig != "" {
		args = append(args, "--kubeconfig", o.Kubeconfig)
	}
	if o.Context != "" {
		args = append(args, "--context", o.Context)
	}
	return args
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	nonNameRe = regexp.MustCompile(`[^a-z0-9-]+`)
)

// Name derives a DNS-1123 Job name from a run id. Kubernetes names are
// lower-case and at most 63 characters; a run id is a ULID, so lower-casing
// keeps it unique.
func Name(runID, suffix string) string {
	n := "harness-" + nonNameRe.ReplaceAllString(strings.ToLower(runID), "-")
	if suffix != "" {
		n += "-" + nonNameRe.ReplaceAllString(strings.ToLower(suffix), "-")
	}
	n = strings.Trim(n, "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	return n
}

// Run describes one worker invocation.
type Run struct {
	// Name is the Job name (see Name).
	Name string
	// RunID and TaskID label the Job so an operator can find it.
	RunID, TaskID string
	// Workspace is the checkout as the orchestrator sees it (under DataRoot).
	Workspace string
	// ConfigDir is the per-run CLAUDE_CONFIG_DIR as the orchestrator sees it.
	ConfigDir string
	// Env are plain KEY=VALUE pairs that are *not* secret (PATH-like toolchain
	// variables). Secrets come from Options.CredentialSecret instead.
	Env []string
	// Cmd is the command inside the container (argv, no shell).
	Cmd []string
	// Deadline bounds the Job independently of the harness's own timeout, so a
	// worker outlives neither. Zero omits activeDeadlineSeconds.
	Deadline time.Duration
}

// Manifest builds the Job as a generic JSON document. JSON is valid YAML, so
// `kubectl create -f -` accepts it as is.
func Manifest(o Options, r Run) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if r.Name == "" {
		return nil, errors.New("k8s: job name is required")
	}
	if r.Workspace == "" {
		return nil, errors.New("k8s: workspace is required")
	}
	if len(r.Cmd) == 0 {
		return nil, errors.New("k8s: command is required")
	}
	wsSub, err := subPath(o.DataRoot, r.Workspace)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"app.kubernetes.io/name": "harness-worker", "harness.role": "worker"}
	for k, v := range o.Labels {
		labels[k] = v
	}
	if r.RunID != "" {
		labels["harness.run"] = Name(r.RunID, "")
	}
	if r.TaskID != "" {
		labels["harness.task"] = Name(r.TaskID, "")
	}

	mounts := []map[string]any{{"name": "data", "mountPath": WorkspaceDir, "subPath": wsSub}}
	env := []map[string]any{{"name": "HOME", "value": HomeDir}}
	if r.ConfigDir != "" {
		cfgSub, err := subPath(o.DataRoot, r.ConfigDir)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, map[string]any{"name": "data", "mountPath": ConfigDir, "subPath": cfgSub})
		env = append(env, map[string]any{"name": "CLAUDE_CONFIG_DIR", "value": ConfigDir})
	}
	volumes := []map[string]any{{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": o.DataClaim}}}
	if o.ReadOnlyRoot {
		mounts = append(mounts,
			map[string]any{"name": "tmp", "mountPath": "/tmp"},
			map[string]any{"name": "home", "mountPath": HomeDir})
		volumes = append(volumes,
			map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
			map[string]any{"name": "home", "emptyDir": map[string]any{}})
	}
	if o.EgressProxy != "" {
		noProxy := o.NoProxy
		if noProxy == "" {
			noProxy = "localhost,127.0.0.1"
		}
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, map[string]any{"name": k, "value": o.EgressProxy})
		}
		env = append(env, map[string]any{"name": "NO_PROXY", "value": noProxy}, map[string]any{"name": "no_proxy", "value": noProxy})
	}
	for _, kv := range r.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.ContainsAny(k, "\n") {
			return nil, fmt.Errorf("k8s: env entry %q must be KEY=VALUE", kv)
		}
		env = append(env, map[string]any{"name": k, "value": v})
	}

	container := map[string]any{
		"name":       "worker",
		"image":      o.Image,
		"command":    r.Cmd,
		"workingDir": WorkspaceDir,
		"env":        env,
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   o.ReadOnlyRoot,
			"capabilities":             map[string]any{"drop": []string{"ALL"}},
		},
		"volumeMounts": mounts,
	}
	if o.ImagePullPolicy != "" {
		container["imagePullPolicy"] = o.ImagePullPolicy
	}
	if o.CredentialSecret != "" {
		container["envFrom"] = []map[string]any{{"secretRef": map[string]any{"name": o.CredentialSecret}}}
	}
	if res := resources(o); len(res) > 0 {
		container["resources"] = res
	}

	podSpec := map[string]any{
		"restartPolicy": "Never",
		// The worker has no business with the API server; not mounting the
		// token means a prompt-injected tool call cannot reach it.
		"automountServiceAccountToken": false,
		"containers":                   []map[string]any{container},
		"volumes":                      volumes,
	}
	if o.ServiceAccount != "" {
		podSpec["serviceAccountName"] = o.ServiceAccount
	}
	if len(o.NodeSelector) > 0 {
		podSpec["nodeSelector"] = o.NodeSelector
	}
	if len(o.Tolerations) > 0 {
		podSpec["tolerations"] = o.Tolerations
	}
	sc := map[string]any{"runAsNonRoot": true}
	if o.RunAsUser > 0 {
		sc["runAsUser"] = o.RunAsUser
	}
	if o.FSGroup > 0 {
		sc["fsGroup"] = o.FSGroup
	}
	podSpec["securityContext"] = sc
	if r.Deadline > 0 {
		podSpec["activeDeadlineSeconds"] = int64(r.Deadline.Seconds())
	}

	ttl := o.TTLAfterFinished
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": r.Name, "namespace": o.namespace(), "labels": labels},
		"spec": map[string]any{
			// The harness owns retries: a Job that retried on its own would
			// spend a second budget and write a second transcript under an id
			// the harness already considers used.
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": int64(ttl.Seconds()),
			"template": map[string]any{
				"metadata": metadata(labels, o.Annotations),
				"spec":     podSpec,
			},
		},
	}
	return json.Marshal(job)
}

func metadata(labels, annotations map[string]string) map[string]any {
	m := map[string]any{"labels": labels}
	if len(annotations) > 0 {
		m["annotations"] = annotations
	}
	return m
}

func resources(o Options) map[string]any {
	req := map[string]any{}
	lim := map[string]any{}
	if o.CPURequest != "" {
		req["cpu"] = o.CPURequest
	}
	if o.MemoryRequest != "" {
		req["memory"] = o.MemoryRequest
	}
	if o.CPULimit != "" {
		lim["cpu"] = o.CPULimit
	}
	if o.MemoryLimit != "" {
		lim["memory"] = o.MemoryLimit
	}
	out := map[string]any{}
	if len(req) > 0 {
		out["requests"] = req
	}
	if len(lim) > 0 {
		out["limits"] = lim
	}
	return out
}

// subPath makes p relative to root for a volume subPath. A path outside the
// data root is refused rather than mounted: a subPath that escapes the claim
// would either fail at the kubelet or mount somebody else's directory.
func subPath(root, p string) (string, error) {
	root = strings.TrimSuffix(path.Clean(root), "/")
	p = path.Clean(p)
	if p == root {
		return ".", nil
	}
	rest, ok := strings.CutPrefix(p, root+"/")
	if !ok {
		return "", fmt.Errorf("k8s: %q is outside the data root %q", p, root)
	}
	if strings.HasPrefix(rest, "..") {
		return "", fmt.Errorf("k8s: %q escapes the data root %q", p, root)
	}
	return rest, nil
}

// CreateArgs builds `kubectl create -f - -o name` (the manifest goes on stdin).
func CreateArgs(o Options) []string {
	return append([]string{o.kubectl()}, append(o.global(), "create", "-f", "-", "-o", "name")...)
}

// LogsArgs builds the command that follows a Job's pod output. It waits for
// the pod to start (--pod-running-timeout) instead of failing while the
// scheduler is still placing it, and asks for the whole log so nothing the
// worker printed before we attached is lost.
func LogsArgs(o Options, name string, startTimeout time.Duration) []string {
	if startTimeout <= 0 {
		startTimeout = 5 * time.Minute
	}
	return append([]string{o.kubectl()}, append(o.global(),
		"logs", "--follow", "--tail=-1", "--pod-running-timeout="+startTimeout.String(), "job/"+name)...)
}

// StatusArgs builds the command that prints the worker container's terminated
// state as JSON, which is where the exit code lives.
func StatusArgs(o Options, name string) []string {
	return append([]string{o.kubectl()}, append(o.global(),
		"get", "pods", "-l", "job-name="+name, "-o",
		`jsonpath={range .items[*]}{.status.containerStatuses[?(@.name=="worker")].state.terminated}{"\n"}{end}`)...)
}

// DeleteArgs builds the command that removes a Job and its pods. grace 0 is
// the SIGKILL equivalent.
func DeleteArgs(o Options, name string, grace time.Duration) []string {
	args := append([]string{o.kubectl()}, o.global()...)
	args = append(args, "delete", "job", name, "--ignore-not-found", "--cascade=background")
	if grace >= 0 {
		args = append(args, fmt.Sprintf("--grace-period=%d", int(grace.Seconds())))
	}
	return args
}

// VersionArgs runs `claude --version` in the worker image as a one-off pod,
// so version pinning checks the image the Jobs will actually use.
func VersionArgs(o Options) []string {
	name := "harness-version-check"
	return append([]string{o.kubectl()}, append(o.global(),
		"run", name, "--rm", "--attach", "--quiet", "--restart=Never",
		"--image", o.Image, "--command", "--", "claude", "--version")...)
}

// TerminatedState is the part of a container status the runner needs.
type TerminatedState struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason"`
	Signal   int    `json:"signal"`
	Message  string `json:"message"`
}

// ParseStatus reads StatusArgs output. Nothing terminated yet (an empty line,
// or a pod still running) returns ok=false, which the caller retries.
func ParseStatus(out []byte) (TerminatedState, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var st TerminatedState
		if err := json.Unmarshal([]byte(line), &st); err != nil {
			continue
		}
		return st, true
	}
	return TerminatedState{}, false
}

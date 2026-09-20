package k8s

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func opts() Options {
	return Options{
		Image: "harness/worker:cli-2.1.243", Namespace: "harness", DataClaim: "harness-data", DataRoot: "/var/lib/harness",
		CredentialSecret: "harness-worker", ServiceAccount: "harness-worker", RunAsUser: 1000, FSGroup: 1000,
		CPURequest: "500m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "4Gi",
		NodeSelector: map[string]string{"harness.io/pool": "workers"}, EgressProxy: "http://egress:3128",
	}
}

func run() Run {
	return Run{
		Name: Name("run_01H8Z", ""), RunID: "run_01H8Z", TaskID: "tsk_01H8Y",
		Workspace: "/var/lib/harness/workspaces/run_01H8Z", ConfigDir: "/var/lib/harness/claude-config/run_01H8Z",
		Cmd: []string{"claude", "-p", "fix the bug"}, Deadline: 15 * time.Minute,
	}
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return m
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, p)
		}
		cur = obj[p]
	}
	return cur
}

func TestManifestShape(t *testing.T) {
	b, err := Manifest(opts(), run())
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, b)
	if dig(t, m, "kind") != "Job" || dig(t, m, "apiVersion") != "batch/v1" {
		t.Errorf("kind/apiVersion: %v %v", dig(t, m, "kind"), dig(t, m, "apiVersion"))
	}
	// The harness owns retries: a Job that retried on its own would spend a
	// second budget and reuse a session id the harness considers consumed.
	if got := dig(t, m, "spec", "backoffLimit"); got != float64(0) {
		t.Errorf("backoffLimit %v, want 0", got)
	}
	pod := dig(t, m, "spec", "template", "spec").(map[string]any)
	if pod["restartPolicy"] != "Never" {
		t.Errorf("restartPolicy %v", pod["restartPolicy"])
	}
	if pod["automountServiceAccountToken"] != false {
		t.Errorf("the worker must not get an API token: %v", pod["automountServiceAccountToken"])
	}
	if pod["activeDeadlineSeconds"] != float64(900) {
		t.Errorf("activeDeadlineSeconds %v", pod["activeDeadlineSeconds"])
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	cmd := c["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "claude" || cmd[2] != "fix the bug" {
		t.Errorf("command %v", cmd)
	}
	sc := c["securityContext"].(map[string]any)
	if sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext %v", sc)
	}
}

// The credential reaches the worker as a Secret reference. A manifest is sent
// to the API server and logged by admission controllers, so a token inlined
// here would be a token in every cluster audit trail.
func TestManifestNeverInlinesASecret(t *testing.T) {
	o := opts()
	r := run()
	r.Env = []string{"GOFLAGS=-mod=mod"}
	b, err := Manifest(o, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-ant") {
		t.Fatal("manifest carries a credential value")
	}
	m := decode(t, b)
	c := dig(t, m, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	envFrom := c["envFrom"].([]any)
	if dig(t, envFrom[0].(map[string]any), "secretRef", "name") != "harness-worker" {
		t.Errorf("envFrom %v", envFrom)
	}
	var names []string
	for _, e := range c["env"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	joined := strings.Join(names, ",")
	for _, secret := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if strings.Contains(joined, secret) {
			t.Errorf("%s is inlined in the manifest env", secret)
		}
	}
	if !strings.Contains(joined, "GOFLAGS") || !strings.Contains(joined, "CLAUDE_CONFIG_DIR") {
		t.Errorf("env %v", names)
	}
}

// The Job mounts exactly the two directories this run owns, as subPaths of the
// shared claim — not the whole data root, which holds every other run's work.
func TestManifestMountsOnlyThisRunsSubPaths(t *testing.T) {
	b, err := Manifest(opts(), run())
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, b)
	c := dig(t, m, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	mounts := c["volumeMounts"].([]any)
	got := map[string]string{}
	for _, mt := range mounts {
		e := mt.(map[string]any)
		got[e["mountPath"].(string)], _ = e["subPath"].(string)
	}
	if got[WorkspaceDir] != "workspaces/run_01H8Z" {
		t.Errorf("workspace subPath %q", got[WorkspaceDir])
	}
	if got[ConfigDir] != "claude-config/run_01H8Z" {
		t.Errorf("config subPath %q", got[ConfigDir])
	}
}

func TestManifestRejectsAPathOutsideTheDataRoot(t *testing.T) {
	r := run()
	r.Workspace = "/etc"
	if _, err := Manifest(opts(), r); err == nil || !strings.Contains(err.Error(), "outside the data root") {
		t.Fatalf("err %v", err)
	}
	r = run()
	r.ConfigDir = "/var/lib/harness/../../etc/shadow"
	if _, err := Manifest(opts(), r); err == nil {
		t.Fatal("a config dir escaping the data root was accepted")
	}
}

func TestValidateRequiresTheSharedVolume(t *testing.T) {
	o := opts()
	o.DataClaim = ""
	if err := o.Validate(); err == nil || !strings.Contains(err.Error(), "data_claim") {
		t.Fatalf("err %v", err)
	}
	o = opts()
	o.Image = ""
	if err := o.Validate(); err == nil {
		t.Fatal("an empty image was accepted")
	}
}

func TestNameIsADNS1123Label(t *testing.T) {
	n := Name("run_01M2M4DGMD2MASZ3DP22JF6E1N", "")
	if !nameRe.MatchString(n) {
		t.Errorf("%q is not a DNS-1123 label", n)
	}
	long := Name(strings.Repeat("run_01H8Z", 20), "worker")
	if len(long) > 63 || !nameRe.MatchString(long) {
		t.Errorf("%q (%d chars)", long, len(long))
	}
}

func TestArgvBuilders(t *testing.T) {
	o := opts()
	o.Kubeconfig = "/home/ops/.kube/config"
	o.Context = "prod"
	create := strings.Join(CreateArgs(o), " ")
	for _, want := range []string{"kubectl", "--namespace harness", "--kubeconfig /home/ops/.kube/config", "--context prod", "create -f -"} {
		if !strings.Contains(create, want) {
			t.Errorf("create args %q missing %q", create, want)
		}
	}
	logs := strings.Join(LogsArgs(o, "harness-x", 90*time.Second), " ")
	if !strings.Contains(logs, "--follow") || !strings.Contains(logs, "--pod-running-timeout=1m30s") || !strings.Contains(logs, "job/harness-x") {
		t.Errorf("logs args %q", logs)
	}
	// Grace 0 is the SIGKILL equivalent and must be explicit, not omitted.
	del := strings.Join(DeleteArgs(o, "harness-x", 0), " ")
	if !strings.Contains(del, "--grace-period=0") || !strings.Contains(del, "--ignore-not-found") {
		t.Errorf("delete args %q", del)
	}
}

func TestParseStatus(t *testing.T) {
	st, ok := ParseStatus([]byte(`{"exitCode":1,"reason":"Error","startedAt":"x"}` + "\n"))
	if !ok || st.ExitCode != 1 || st.Reason != "Error" {
		t.Errorf("%+v ok=%v", st, ok)
	}
	// A pod that has not terminated prints nothing; that is "not yet", not an
	// exit code of zero — reporting 0 would mark a running worker as success.
	if _, ok := ParseStatus([]byte("\n \n")); ok {
		t.Error("empty status parsed as terminated")
	}
	if _, ok := ParseStatus([]byte("no resources found")); ok {
		t.Error("non-JSON status parsed as terminated")
	}
}

// TestWorkerJobGolden keeps a readable copy of the Job the harness submits.
// It is what `scripts/k8s-validate.sh` runs kubeconform against, so a manifest
// that stopped being valid Kubernetes fails here rather than at the API
// server, on the first paid run of a cluster deployment.
//
//	go test ./internal/k8s -update   # after a deliberate change
func TestWorkerJobGolden(t *testing.T) {
	b, err := Manifest(opts(), run())
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", "worker-job.json")
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if string(want) != pretty.String() {
		t.Errorf("worker Job manifest changed; review and rerun with -update:\n--- want\n%s\n--- got\n%s", want, pretty.String())
	}
}

var update = flag.Bool("update", false, "rewrite the golden worker Job manifest")

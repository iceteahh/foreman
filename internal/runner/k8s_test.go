package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/k8s"
)

// fakeKubectl stands in for the kubectl client. It records every invocation
// under $REC (one file per subcommand, appended) and decides what `delete` and
// `get` do from control files the test writes:
//
//	$REC/delete-fails   — `delete` exits non-zero, as an RBAC denial would
//	$REC/job-remains    — `get job` keeps printing the Job, so the delete is
//	                      accepted but the Job never actually goes away
func fakeKubectl(t *testing.T) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	if err := os.MkdirAll(rec, 0o750); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
REC="` + rec + `"
sub=""
for a in "$@"; do
  case "$a" in
    delete|get|create|logs) sub="$a"; break ;;
  esac
done
printf '%s\n' "$*" >> "$REC/$sub"
case "$sub" in
  delete)
    if [ -f "$REC/delete-fails" ]; then
      echo 'Error from server (Forbidden): jobs.batch is forbidden' >&2
      exit 1
    fi
    exit 0
    ;;
  get)
    if [ -f "$REC/job-remains" ]; then echo "job.batch/harness-run_x"; fi
    exit 0
    ;;
esac
exit 0
`
	bin = filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, rec
}

func k8sProc(t *testing.T, bin string, killTimeout time.Duration) *k8sProcess {
	t.Helper()
	l := &K8sLauncher{
		Options:     k8s.Options{Kubectl: bin, Namespace: "harness", Image: "img", DataClaim: "c", DataRoot: t.TempDir()},
		KillTimeout: killTimeout,
		StatusPoll:  10 * time.Millisecond,
		Grace:       time.Second,
	}
	return &k8sProcess{launcher: l, name: "harness-run_x", cmd: nil, exited: make(chan struct{})}
}

func countCalls(t *testing.T, rec, sub string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rec, sub))
	if err != nil {
		return 0
	}
	return len(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
}

// A kubectl delete that fails must surface as a Signal error. Swallowing it is
// how a runaway Job keeps spending while the harness reports the worker killed.
func TestK8sSignalReportsAFailedDelete(t *testing.T) {
	bin, rec := fakeKubectl(t)
	if err := os.WriteFile(filepath.Join(rec, "delete-fails"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := k8sProc(t, bin, 100*time.Millisecond)
	err := p.Signal(syscall.SIGTERM)
	if err == nil {
		t.Fatal("a failed kubectl delete was reported as a successful kill")
	}
	if !strings.Contains(err.Error(), "harness-run_x") || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("the error does not name the Job or the cause: %v", err)
	}
	if err := p.Signal(syscall.SIGKILL); err == nil {
		t.Fatal("SIGKILL reported success on a failed delete")
	}
}

// A delete the API accepted is not a Job that is gone. SIGKILL only reports
// success once the Job reads NotFound.
func TestK8sSignalConfirmsTheJobIsGone(t *testing.T) {
	bin, rec := fakeKubectl(t)
	p := k8sProc(t, bin, 100*time.Millisecond)
	if err := p.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("a Job that is gone must confirm: %v", err)
	}
	if countCalls(t, rec, "get") == 0 {
		t.Error("SIGKILL did not check whether the Job was actually gone")
	}

	// Now the Job survives its own deletion: the kill is unconfirmed.
	bin2, rec2 := fakeKubectl(t)
	if err := os.WriteFile(filepath.Join(rec2, "job-remains"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := k8sProc(t, bin2, 100*time.Millisecond)
	err := p2.Signal(syscall.SIGKILL)
	if err == nil {
		t.Fatal("a Job that never went away was reported as killed")
	}
	if !errors.Is(err, ErrWorkerOrphaned) {
		t.Errorf("an unconfirmed kill must be ErrWorkerOrphaned so the pool pages: %v", err)
	}
	if !strings.Contains(err.Error(), "harness-run_x") {
		t.Errorf("the error does not name the Job an operator has to delete: %v", err)
	}
	if n := countCalls(t, rec2, "get"); n < 2 {
		t.Errorf("gave up after %d status checks; it must poll until the timeout", n)
	}
}

// SIGTERM deletes with a grace period and does not wait for confirmation: the
// kubelet is still stopping the container, and the SIGKILL that follows is what
// confirms.
func TestK8sSigtermDeletesWithGraceAndDoesNotBlock(t *testing.T) {
	bin, rec := fakeKubectl(t)
	p := k8sProc(t, bin, time.Minute)
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(rec, "delete"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "--grace-period=1") {
		t.Errorf("SIGTERM must delete with a grace period: %s", b)
	}
	if countCalls(t, rec, "get") != 0 {
		t.Error("SIGTERM must not wait for the Job to disappear")
	}
}

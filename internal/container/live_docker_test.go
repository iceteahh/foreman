//go:build docker

// These tests need a real Docker daemon and the worker image
// (`make worker-image`). They spend no tokens. Run them with `make docker-test`.
package container_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/container"
)

const (
	image   = "harness/worker:cli-2.1.270"
	network = "harness-test-internal"
	// bridge is the second network the proxy joins so it can reach the internet.
	bridge = "harness-test-egress"
)

func run(t *testing.T, timeout time.Duration, argv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	return string(out), code
}

func requireImage(t *testing.T) {
	t.Helper()
	if _, code := run(t, 30*time.Second, "docker", "image", "inspect", image); code != 0 {
		t.Skipf("worker image %s is not built (make worker-image)", image)
	}
}

// TestWorkerImageRunsPinnedCLI checks the image the harness pins actually
// carries that CLI and the toolchains the code_fix kind needs.
func TestWorkerImageRunsPinnedCLI(t *testing.T) {
	requireImage(t)
	out, code := run(t, 60*time.Second, container.VersionArgs(container.Options{Image: image})...)
	if code != 0 {
		t.Fatalf("version exited %d: %s", code, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "2.1.270") {
		t.Errorf("image CLI version %q", out)
	}
	ws := t.TempDir()
	argv, err := container.Args(container.Options{Image: image, Network: "none"},
		container.Run{Workspace: ws, Cmd: []string{"sh", "-c", "id -u; go version; git --version; echo $HOME; touch /workspace/written"}})
	if err != nil {
		t.Fatal(err)
	}
	out, code = run(t, 120*time.Second, argv...)
	if code != 0 {
		t.Fatalf("toolchain probe exited %d: %s", code, out)
	}
	for _, want := range []string{"go version go1.", "git version", container.HomeDir} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.HasPrefix(out, "0\n") {
		t.Error("worker runs as root")
	}
	// The bind mount is writable from inside, which is what lets the worker edit the checkout.
	if _, err := os.Stat(ws + "/written"); err != nil {
		t.Errorf("workspace bind mount not writable from the container: %v", err)
	}
}

// TestNetworkNoneHasNoEgress is the baseline: with --network none nothing gets out.
func TestNetworkNoneHasNoEgress(t *testing.T) {
	requireImage(t)
	argv, err := container.Args(container.Options{Image: image, Network: "none"},
		container.Run{Workspace: t.TempDir(), Cmd: []string{"curl", "-sS", "--max-time", "10", "https://example.com/"}})
	if err != nil {
		t.Fatal(err)
	}
	out, code := run(t, 60*time.Second, argv...)
	if code == 0 {
		t.Errorf("curl succeeded with --network none:\n%s", out)
	}
}

// TestEgressAllowlistBlocksNonAllowlistedHost is the Step 14 acceptance test:
// a worker on the internal network reaches an allowlisted host through the
// proxy and is refused for anything else, with no direct route out.
func TestEgressAllowlistBlocksNonAllowlistedHost(t *testing.T) {
	requireImage(t)
	// Networks: `internal` has no route off the host; `bridge` does.
	run(t, 30*time.Second, "docker", "network", "rm", network)
	run(t, 30*time.Second, "docker", "network", "rm", bridge)
	if out, code := run(t, 30*time.Second, "docker", "network", "create", "--internal", network); code != 0 {
		t.Fatalf("create internal network: %s", out)
	}
	if out, code := run(t, 30*time.Second, "docker", "network", "create", bridge); code != 0 {
		t.Fatalf("create bridge network: %s", out)
	}
	t.Cleanup(func() {
		run(t, 30*time.Second, "docker", "rm", "-f", "harness-test-egress-proxy")
		run(t, 30*time.Second, "docker", "network", "rm", network)
		run(t, 30*time.Second, "docker", "network", "rm", bridge)
	})

	// The proxy runs `harness egress` from the repo with the Go image, joined to
	// both networks: workers reach it by the alias "egress".
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo = strings.TrimSuffix(repo, "/internal/container")
	if out, code := run(t, 30*time.Second, "docker", "run", "-d", "--name", "harness-test-egress-proxy",
		"--network", network, "--network-alias", "egress",
		"-v", repo+":/src", "-w", "/src", "-e", "GOFLAGS=-mod=mod", "-e", "GOCACHE=/tmp/gocache", "-e", "GOMODCACHE=/tmp/gomod",
		"golang:1.27-bookworm", "go", "run", "./cmd/harness", "egress", "-addr", ":3128"); code != 0 {
		t.Fatalf("start proxy: %s", out)
	}
	if out, code := run(t, 30*time.Second, "docker", "network", "connect", bridge, "harness-test-egress-proxy"); code != 0 {
		t.Fatalf("connect proxy to the bridge: %s", out)
	}
	// `go run` compiles first; wait for the listener.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		logs, _ := run(t, 30*time.Second, "docker", "logs", "harness-test-egress-proxy")
		if strings.Contains(logs, "egress proxy listening") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start:\n%s", logs)
		}
		time.Sleep(2 * time.Second)
	}

	opts := container.Options{Image: image, Network: network, EgressProxy: "http://egress:3128"}
	curl := func(url string) (string, int) {
		argv, err := container.Args(opts, container.Run{Workspace: t.TempDir(),
			Cmd: []string{"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "30", url}})
		if err != nil {
			t.Fatal(err)
		}
		return run(t, 90*time.Second, argv...)
	}

	// Not allowlisted: the proxy refuses the tunnel, so curl fails.
	out, code := curl("https://example.com/")
	if code == 0 {
		t.Errorf("curl to a non-allowlisted host succeeded (http_code %q)", out)
	}
	if !strings.Contains(out, "403") && !strings.Contains(strings.ToLower(out), "forbidden") && !strings.Contains(out, "CONNECT") {
		t.Logf("denied curl output (exit %d): %s", code, out)
	}

	// Allowlisted: the tunnel is established and the API answers (401 without a key).
	out, code = curl("https://api.anthropic.com/v1/messages")
	if code != 0 {
		t.Errorf("curl to an allowlisted host failed (exit %d): %s", code, out)
	}
	if !strings.Contains(out, "40") && !strings.Contains(out, "200") {
		t.Errorf("unexpected status from the allowlisted host: %q", out)
	}

	// And with no proxy configured there is no route out at all: the internal
	// network is the enforcement boundary, the proxy only decides what passes.
	argv, err := container.Args(container.Options{Image: image, Network: network},
		container.Run{Workspace: t.TempDir(), Cmd: []string{"curl", "-sS", "--max-time", "15", "https://example.com/"}})
	if err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, 60*time.Second, argv...); code == 0 {
		t.Errorf("direct egress from the internal network succeeded:\n%s", out)
	}

	logs, _ := run(t, 30*time.Second, "docker", "logs", "harness-test-egress-proxy")
	if !strings.Contains(logs, "egress denied") {
		t.Errorf("proxy did not log a denial:\n%s", logs)
	}
	if !strings.Contains(logs, "egress allowed") {
		t.Errorf("proxy did not log the allowed tunnel:\n%s", logs)
	}
}

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/config"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/session"
	storepostgres "github.com/100xteam-ai/foreman/internal/store/postgres"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, cfgErr := config.Load(*cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	failed := 0
	report := func(ok bool, name, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-14s %s\n", mark, name, detail)
	}
	report(true, "go", runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		report(false, "git", err.Error())
	} else {
		report(true, "git", strings.TrimSpace(string(out)))
	}
	report(cfgErr == nil, "config", *cfgPath+configDetail(cfgErr))

	// The CLI that matters is the one the configured worker mode runs: the host
	// binary, or the one baked into the worker image.
	launcher := workerLauncher(cfg)
	if v, err := launcher.Version(ctx); err != nil {
		report(false, "claude", launcher.Name()+": "+err.Error())
	} else if v != runner.PinnedCLIVersion {
		report(false, "claude", fmt.Sprintf("%s reports %s, pinned %s (re-run the Step 4 spike before bumping)", launcher.Name(), v, runner.PinnedCLIVersion))
	} else {
		report(true, "claude", v+" (pinned, "+launcher.Name()+")")
	}
	if cfg.Worker.Mode == "k8s" {
		kubectl := cfg.Worker.K8s.Kubectl
		if kubectl == "" {
			kubectl = "kubectl"
		}
		if out, err := exec.CommandContext(ctx, kubectl, "version", "--client", "-o", "yaml").Output(); err != nil {
			report(false, "kubectl", err.Error())
		} else {
			report(true, "kubectl", firstNonEmptyLine(string(out)))
		}
		// The worker Job mounts subPaths of this claim; without it every run
		// fails at the kubelet, one scheduling round-trip after it was queued.
		claim := cfg.Worker.K8s.DataClaim
		args := append([]string{"get", "pvc", claim, "-o", "jsonpath={.status.accessModes[*]} {.status.phase}"}, k8sNamespaceArgs(cfg)...)
		out, err := exec.CommandContext(ctx, kubectl, args...).Output()
		switch {
		case err != nil:
			report(false, "worker volume", claim+": "+err.Error())
		case !strings.Contains(string(out), "ReadWriteMany"):
			// ReadWriteOnce binds every worker Job to the orchestrator's node
			// and stalls the moment you scale out — a failure that looks like
			// "pods pending" long after the rollout that caused it.
			fmt.Printf("%s  %-14s %s\n", "warn", "worker volume",
				claim+" is "+strings.TrimSpace(string(out))+"; worker Jobs need ReadWriteMany to mount the checkout from another node")
		default:
			report(true, "worker volume", claim+" ("+strings.TrimSpace(string(out))+")")
		}
		if cfg.Worker.K8s.CredentialSecret == "" {
			report(false, "worker secret", "worker.k8s.credential_secret is empty: worker Jobs get no credential")
		} else {
			args := append([]string{"get", "secret", cfg.Worker.K8s.CredentialSecret, "-o", "name"}, k8sNamespaceArgs(cfg)...)
			if err := exec.CommandContext(ctx, kubectl, args...).Run(); err != nil {
				report(false, "worker secret", cfg.Worker.K8s.CredentialSecret+" not found in namespace "+cfg.Worker.K8s.Namespace)
			} else {
				report(true, "worker secret", cfg.Worker.K8s.CredentialSecret)
			}
		}
	}
	if cfg.Database.Driver == "postgres" {
		if cfg.PostgresDSN() == "" {
			report(false, "postgres", "no DSN in database.dsn or $"+cfg.Database.DSNEnv)
		} else {
			st, err := storepostgres.Open(ctx, cfg.PostgresDSN())
			if err != nil {
				report(false, "postgres", err.Error())
			} else {
				_ = st.Close()
				report(true, "postgres", "reachable, schema up to date")
			}
		}
	}
	if cfg.Session.Store == "s3" {
		if _, err := session.NewS3(cfg.SessionS3Config()); err != nil {
			report(false, "session store", err.Error())
		} else {
			report(true, "session store", "s3://"+cfg.Session.Bucket+" at "+cfg.Session.Endpoint)
		}
	}
	if cfg.Worker.Mode == "docker" {
		if out, err := exec.CommandContext(ctx, dockerBin(cfg), "version", "--format", "{{.Server.Version}}").Output(); err != nil {
			report(false, "docker", err.Error())
		} else {
			report(true, "docker", "server "+strings.TrimSpace(string(out)))
		}
		net := cfg.Worker.Docker.Network
		switch net {
		case "", "none":
			fmt.Printf("%s  %-14s %s\n", "warn", "worker network", "no network isolation configured (worker.docker.network)")
		default:
			out, err := exec.CommandContext(ctx, dockerBin(cfg), "network", "inspect", net, "--format", "{{.Internal}}").Output()
			switch {
			case err != nil:
				report(false, "worker network", net+" not found: create it with `docker network create --internal "+net+"`")
			case strings.TrimSpace(string(out)) != "true":
				fmt.Printf("%s  %-14s %s\n", "warn", "worker network", net+" is not --internal: workers can reach the internet directly, bypassing the egress allowlist")
			default:
				report(true, "worker network", net+" (internal)")
			}
		}
	}
	switch {
	case os.Getenv(cfg.Worker.APIKeyEnv) != "":
		report(true, "credential", cfg.Worker.APIKeyEnv+" set (API key)")
	case os.Getenv(cfg.Worker.OAuthTokenEnv) != "":
		report(true, "credential", cfg.Worker.OAuthTokenEnv+" set (OAuth token from `claude setup-token`)")
	default:
		report(false, "credential", fmt.Sprintf("neither %s nor %s is set: workers cannot authenticate (host login is Keychain-only)", cfg.Worker.APIKeyEnv, cfg.Worker.OAuthTokenEnv))
	}
	switch {
	case cfg.APIToken() != "":
		report(true, "api auth", cfg.Server.APITokenEnv+" set; every route but /healthz and the webhook requires it")
	case cfg.APIAuthError() == nil:
		fmt.Printf("%s  %-14s %s\n", "warn", "api auth", cfg.Server.APITokenEnv+" unset: the API is open, allowed only because "+cfg.Server.Addr+" is loopback")
	default:
		report(false, "api auth", cfg.Server.APITokenEnv+" unset and "+cfg.Server.Addr+" is not loopback: `serve` refuses to start")
	}
	fmt.Printf("%s  %-14s %s\n", warnMark(os.Getenv(cfg.GitHub.TokenEnv) != ""), "github token", cfg.GitHub.TokenEnv+envDetail(cfg.GitHub.TokenEnv, "delivery will push branches without opening PRs"))
	fmt.Printf("%s  %-14s %s\n", warnMark(os.Getenv(cfg.GitHub.WebhookSecretEnv) != ""), "webhook secret", cfg.GitHub.WebhookSecretEnv+envDetail(cfg.GitHub.WebhookSecretEnv, "signatures not verified"))
	switch {
	case os.Getenv(cfg.Review.SlackBotTokenEnv) == "":
		fmt.Printf("%s  %-14s %s\n", "warn", "slack review", cfg.Review.SlackBotTokenEnv+" unset: review requests are logged; decide with `harness review` or POST /runs/{id}/review")
	case os.Getenv(cfg.Review.SlackSigningSecretEnv) == "":
		fmt.Printf("%s  %-14s %s\n", "warn", "slack review", "posting to "+cfg.Review.SlackChannel+"; "+cfg.Review.SlackSigningSecretEnv+" unset so buttons (/slack/actions) are disabled")
	default:
		fmt.Printf("%s  %-14s %s\n", "ok  ", "slack review", "posting to "+cfg.Review.SlackChannel+", interactivity enabled")
	}
	if err := os.MkdirAll(cfg.DataRoot, 0o750); err != nil {
		report(false, "data root", cfg.DataRoot+": "+err.Error())
	} else {
		report(true, "data root", cfg.DataRoot)
	}
	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	return nil
}

// workerLauncher mirrors the app's launcher choice so doctor checks the same
// CLI the pool would run.
func workerLauncher(cfg config.Config) runner.Launcher {
	switch cfg.Worker.Mode {
	case "docker":
		return &runner.DockerLauncher{Options: cfg.ContainerOptions()}
	case "k8s":
		return &runner.K8sLauncher{Options: cfg.K8sOptions()}
	}
	return &runner.LocalLauncher{Bin: cfg.Worker.ClaudeBin}
}

func k8sNamespaceArgs(cfg config.Config) []string {
	ns := cfg.Worker.K8s.Namespace
	if ns == "" {
		return nil
	}
	return []string{"--namespace", ns}
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

func dockerBin(cfg config.Config) string {
	if b := cfg.Worker.Docker.Bin; b != "" {
		return b
	}
	return "docker"
}

func warnMark(ok bool) string {
	if ok {
		return "ok  "
	}
	return "warn"
}

func configDetail(err error) string {
	if err != nil {
		return ": " + err.Error()
	}
	return ""
}

func envDetail(name, consequence string) string {
	if os.Getenv(name) != "" {
		return " set"
	}
	return " unset: " + consequence
}

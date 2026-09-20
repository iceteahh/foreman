package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// PinnedCLIVersion is the Claude Code CLI version this harness was verified against.
// docs/cli-contract.md describes the behaviour that was checked. Bump it only after
// re-running the spike and, once Step 19 exists, the golden suite.
const PinnedCLIVersion = "2.1.270"

// DefaultBin is the executable name used when no explicit path is configured.
const DefaultBin = "claude"

// CLIVersion runs `<bin> --version` and returns the bare version string
// (e.g. "2.1.270" from "2.1.270 (Claude Code)").
func CLIVersion(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("run %s --version: %w", bin, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s --version printed nothing", bin)
	}
	return fields[0], nil
}

// CheckVersion fails when the installed CLI does not match PinnedCLIVersion.
func CheckVersion(ctx context.Context, bin string) error {
	v, err := CLIVersion(ctx, bin)
	if err != nil {
		return err
	}
	if v != PinnedCLIVersion {
		return fmt.Errorf("claude CLI version drift: installed %s, pinned %s", v, PinnedCLIVersion)
	}
	return nil
}

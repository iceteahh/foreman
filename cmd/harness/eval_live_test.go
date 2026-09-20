//go:build live

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/golden"
)

// TestLiveGoldenSlice runs a few golden cases through the real pipeline with
// the real CLI. It spends tokens (a few cents per case on haiku) and is the
// only test that exercises appExecutor, the seam the whole suite depends on:
// everything else fakes it. Run with `make live-golden`.
//
// GOLDEN_CASES selects which cases run; the default is one cheap case per
// kind, so a broken template for any kind shows up here rather than in the
// nightly bill.
func TestLiveGoldenSlice(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		t.Skip("neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set")
	}
	ids := []string{"code-fix-add-function", "code-review-approve", "triage-needs-info", "report-repo-overview"}
	if v := os.Getenv("GOLDEN_CASES"); v != "" {
		ids = splitList(v)
	}
	suite, err := golden.Load("../../evals/golden")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := filepath.Join(root, "harness.yaml")
	// check_version is off because the host CLI drifts on its own schedule;
	// the pinned version is enforced in CI, where the workflow installs it.
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`
data_root: %s
concurrency: { global: 1 }
worker: { check_version: false }
judge: { model: haiku, max_turns: 5, max_cost_usd: 0.15 }
observability: { progress: off, prometheus_addr: "" }
env: { GOFLAGS: "-mod=mod", GOCACHE: "%s", GOPATH: "%s" }
`, filepath.Join(root, "data"), os.Getenv("GOCACHE"), os.Getenv("GOPATH"))), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app, err := buildApp(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	r := &golden.Runner{
		Suite: suite, Executor: &appExecutor{app: app}, Select: ids,
		MaxCostUSD: 3, CaseTimeout: 10 * time.Minute, WorkRoot: filepath.Join(root, "fixtures"), Logger: logger,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	rep, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rep.Render(os.Stdout)
	if rep.Totals.Scored != len(ids) {
		t.Fatalf("scored %d of %d selected cases", rep.Totals.Scored, len(ids))
	}
	for _, c := range rep.Cases {
		if c.Skipped {
			continue
		}
		if !c.OK {
			t.Errorf("%s: %v", c.ID, c.Reasons)
		}
	}
	t.Logf("live golden slice: %d/%d passed, $%.4f", rep.Totals.Passed, rep.Totals.Scored, rep.Totals.CostUSD)
}

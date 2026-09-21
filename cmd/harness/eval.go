package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/golden"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

const evalUsage = `usage: harness eval <subcommand> [flags]

subcommands:
  run [dir]         execute the golden suite and write a scored report
  gate              compare a report against a baseline (the regression gate)
  list [dir]        list the cases in a suite without running them
  import-reviews    turn human review overrides into golden case skeletons
`

func cmdEval(args []string, logger *slog.Logger) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, evalUsage)
		return errors.New("eval: a subcommand is required")
	}
	switch args[0] {
	case "run":
		return cmdEvalRun(args[1:], logger)
	case "gate":
		return cmdEvalGate(args[1:])
	case "list":
		return cmdEvalList(args[1:])
	case "import-reviews":
		return cmdEvalImport(args[1:], logger)
	case "-h", "--help", "help":
		fmt.Print(evalUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, evalUsage)
		return fmt.Errorf("eval: unknown subcommand %q", args[0])
	}
}

// cmdEvalRun executes the golden suite through the real pipeline (plan Step
// 19). It spends tokens: the cost cap is the reason it is safe to schedule.
func cmdEvalRun(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	out := fs.String("out", "", "write the report here (default evals/reports/<date>.json)")
	reportDir := fs.String("report-dir", "evals/reports", "directory for the default report path")
	maxCost := fs.Float64("max-cost", 10, "stop the suite once this many dollars are spent (0 = no cap)")
	caseTimeout := fs.Duration("case-timeout", 30*time.Minute, "give up on one case after this long")
	selected := fs.String("select", "", "comma-separated case ids to run (default all)")
	tags := fs.String("tag", "", "comma-separated tags; a case runs if it carries any of them")
	baseline := fs.String("baseline", "", "also apply the regression gate against this report")
	maxDrop := fs.Float64("max-drop", golden.DefaultLimits().MaxPassRateDrop, "allowed pass-rate drop with -baseline (0.05 = 5 points)")
	keep := fs.Bool("keep-fixtures", false, "leave the materialised fixture repos on disk")
	repeat := fs.Int("repeat", 1, "run each case this many times and score the majority verdict (cost scales with it)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness eval run [evals/golden] [-config …] [-out …] [-max-cost 10] [-repeat 1] [-select id,…] [-tag …] [-baseline report.json]")
		fs.PrintDefaults()
	}
	dir, args := takeDir(args, "evals/golden")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("eval run: unexpected argument %q (the suite directory goes first)", fs.Arg(0))
	}
	suite, err := golden.Load(dir)
	if err != nil {
		return err
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()

	ex := &appExecutor{app: app}
	r := &golden.Runner{
		Suite: suite, Executor: ex, MaxCostUSD: *maxCost, CaseTimeout: *caseTimeout,
		Select: splitList(*selected), Tags: splitList(*tags), KeepFixtures: *keep, Repeat: *repeat, Logger: logger,
		HarnessVersion: version, CLIVersion: runner.PinnedCLIVersion, WorkerMode: app.Config.Worker.Mode,
		OnCase: func(c golden.CaseResult) {
			status := "ok"
			if !c.OK {
				status = "FAIL"
			}
			fmt.Printf("%-5s %-32s %-13s $%.4f\n", status, c.ID, c.Actual, c.CostUSD)
		},
	}

	ctx, cancel := signalContext(context.Background())
	defer cancel()
	fmt.Printf("golden suite %s: %d case(s), cost cap $%.2f, worker mode %s\n\n", dir, len(suite.Cases), *maxCost, app.Config.Worker.Mode)
	rep, err := r.Run(ctx)
	if rep == nil {
		return err
	}
	path := *out
	if path == "" {
		path = golden.DefaultReportPath(*reportDir, time.Now())
	}
	if werr := rep.Write(path); werr != nil {
		return errors.Join(err, werr)
	}
	fmt.Println()
	rep.Render(os.Stdout)
	fmt.Printf("\nreport written to %s\n", path)
	if err != nil {
		return err
	}
	if *baseline == "" {
		if rep.Totals.Failed > 0 {
			return fmt.Errorf("%d golden case(s) failed", rep.Totals.Failed)
		}
		return nil
	}
	base, err := golden.ReadReport(*baseline)
	if err != nil {
		return err
	}
	g := golden.Gate(base, rep, golden.Limits{MaxPassRateDrop: *maxDrop})
	fmt.Println()
	g.Render(os.Stdout)
	if !g.OK {
		return errors.New("golden regression gate blocked this change")
	}
	return nil
}

// cmdEvalGate applies the regression gate to reports that already exist, which
// is how CI blocks a prompt or policy change (plan Step 20).
func cmdEvalGate(args []string) error {
	fs := flag.NewFlagSet("eval gate", flag.ContinueOnError)
	baseline := fs.String("baseline", "", "the report the change is measured against")
	report := fs.String("report", "", "the report produced by this change")
	maxDrop := fs.Float64("max-drop", golden.DefaultLimits().MaxPassRateDrop, "allowed pass-rate drop (0.05 = 5 points)")
	minRate := fs.Float64("min-pass-rate", 0, "absolute pass-rate floor (0 disables)")
	allowNew := fs.Bool("allow-new-failures", false, "permit a case that passed in the baseline to fail now")
	allowMissing := fs.Bool("allow-missing-cases", false, "permit baseline cases to be absent from the report")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness eval gate -baseline <report.json> -report <report.json> [-max-drop 0.05]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *report == "" {
		fs.Usage()
		return errors.New("eval gate: -report is required")
	}
	cur, err := golden.ReadReport(*report)
	if err != nil {
		return err
	}
	var base *golden.Report
	if *baseline != "" {
		if base, err = golden.ReadReport(*baseline); err != nil {
			return err
		}
	}
	g := golden.Gate(base, cur, golden.Limits{
		MaxPassRateDrop: *maxDrop, MinPassRate: *minRate,
		AllowNewFailures: *allowNew, AllowMissingCases: *allowMissing,
	})
	g.Render(os.Stdout)
	if !g.OK {
		return errors.New("golden regression gate blocked this change")
	}
	return nil
}

// cmdEvalList shows what the suite covers without spending anything.
func cmdEvalList(args []string) error {
	fs := flag.NewFlagSet("eval list", flag.ContinueOnError)
	dir, args := takeDir(args, "evals/golden")
	if err := fs.Parse(args); err != nil {
		return err
	}
	suite, err := golden.Load(dir)
	if err != nil {
		return err
	}
	for _, c := range suite.Cases {
		mark := " "
		if c.Skip != "" {
			mark = "S"
		}
		fmt.Printf("%s %-32s %-18s %-13s %v\n", mark, c.ID, c.Kind, c.Expect.Outcome, c.Tags)
		fmt.Printf("    %s\n", c.Description)
	}
	fmt.Printf("\n%d case(s) in %s\n", len(suite.Cases), dir)
	return nil
}

// cmdEvalImport turns human review overrides into golden case skeletons
// (design §5.4 feedback loop). The fixture repository cannot be recovered from
// a decision, so each skeleton lands with a `skip` note telling the operator
// what to capture.
func cmdEvalImport(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("eval import-reviews", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	outDir := fs.String("out", "evals/golden", "write case skeletons here")
	since := fs.Duration("since", 30*24*time.Hour, "only decisions this recent")
	limit := fs.Int("limit", 200, "maximum decisions to scan")
	dryRun := fs.Bool("dry-run", false, "list the overrides without writing files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	recs, err := app.Store.ListDecisionsSince(ctx, time.Now().Add(-*since), *limit)
	if err != nil {
		return err
	}
	written := 0
	for _, rec := range recs {
		if !golden.IsOverride(rec) {
			continue
		}
		run, err := app.Store.GetRun(ctx, rec.Decision.RunID)
		if err != nil {
			logger.Warn("skipping an override whose run is gone", "run_id", rec.Decision.RunID, "err", err)
			continue
		}
		t, err := app.Store.GetTask(ctx, run.TaskID)
		if err != nil {
			logger.Warn("skipping an override whose task is gone", "task_id", run.TaskID, "err", err)
			continue
		}
		c, err := golden.FromOverride(golden.Override{Decision: rec, Task: t, Run: run})
		if err != nil {
			return err
		}
		path := filepath.Join(*outDir, c.ID+".json")
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("exists  %s\n", path)
			continue
		}
		if *dryRun {
			fmt.Printf("would write %s — %s\n", path, c.Description)
			continue
		}
		if err := golden.WriteCase(path, c); err != nil {
			return err
		}
		fmt.Printf("wrote   %s — %s\n", path, c.Description)
		written++
	}
	if *dryRun {
		return nil
	}
	if written == 0 {
		fmt.Printf("no new overrides in the last %s (%d decision(s) scanned)\n", *since, len(recs))
		return nil
	}
	fmt.Printf("\n%d skeleton(s) written to %s. Each needs its fixture repo captured before the `skip` can be removed.\n", written, *outDir)
	return nil
}

// appExecutor runs a golden case through the real pipeline: the same
// submitter, pool, checks, judge, router and delivery a production task uses.
// Anything less would evaluate a different system than the one that ships.
type appExecutor struct {
	app *app
}

// Execute implements golden.Executor.
func (e *appExecutor) Execute(ctx context.Context, spec task.Spec) (*golden.Execution, error) {
	start := time.Now()
	// The workspace has to survive the run so the case can assert which files
	// were touched; it is destroyed below once the capture is taken.
	prevKeep := e.app.Pool.Config.KeepWorkspaces
	e.app.Pool.Config.KeepWorkspaces = true
	defer func() { e.app.Pool.Config.KeepWorkspaces = prevKeep }()

	t, first, err := e.app.Submit.Submit(ctx, spec)
	if err != nil {
		return &golden.Execution{DurationMS: time.Since(start).Milliseconds()}, err
	}
	e.app.Logger.Info("golden case submitted", "task_id", t.ID, "run_id", first.ID, "kind", string(spec.Kind))
	final, runErr := e.app.DriveTask(ctx, t.ID)

	// Read back from the store: statuses, metrics and evals are authoritative
	// there, and a retried case has several runs to account for.
	sctx := context.WithoutCancel(ctx)
	ex := &golden.Execution{Task: t, Final: final, DurationMS: time.Since(start).Milliseconds(), Err: runErr}
	if t, err := e.app.Store.GetTask(sctx, t.ID); err == nil {
		ex.Task = t
	}
	if runs, err := e.app.Store.ListRunsByTask(sctx, t.ID); err == nil {
		ex.Runs = runs
		if final == nil && len(runs) > 0 {
			ex.Final = runs[len(runs)-1]
		}
	}
	if ex.Final != nil {
		ex.ChangedFiles = e.changedFiles(sctx, ex.Task, ex.Final)
	}
	e.destroyWorkspaces(sctx, ex.Task, ex.Runs)
	return ex, nil
}

// changedFiles reopens the run's workspace and reports what the worker
// touched. A delivered run has already been committed and pushed, so the
// capture is taken against the base sha either way.
func (e *appExecutor) changedFiles(ctx context.Context, t *task.Task, r *task.Run) []string {
	ws, err := e.app.Pool.Workspaces.Open(ctx, t, r)
	if err != nil {
		if !errors.Is(err, workspace.ErrNoWorkspace) {
			e.app.Logger.Warn("golden: reopening the workspace failed", "run_id", r.ID, "err", err)
		}
		return nil
	}
	c, err := ws.Capture(ctx)
	if err != nil {
		e.app.Logger.Warn("golden: capturing the workspace failed", "run_id", r.ID, "err", err)
		return nil
	}
	return c.ChangedFiles
}

// destroyWorkspaces removes the checkouts the case forced the pool to keep.
// A suite of 20 cases would otherwise leave 20 clones behind per run.
func (e *appExecutor) destroyWorkspaces(ctx context.Context, t *task.Task, runs []*task.Run) {
	if e.app.Config.Retention.Workspaces == "keep" {
		return
	}
	for _, r := range runs {
		ws, err := e.app.Pool.Workspaces.Open(ctx, t, r)
		if err != nil {
			continue
		}
		if err := ws.Destroy(); err != nil {
			e.app.Logger.Warn("golden: destroying the workspace failed", "run_id", r.ID, "err", err)
		}
	}
}

// takeDir pulls a leading non-flag argument off args and returns it with the
// rest. Go's flag package stops at the first positional, so
// `eval run evals/golden -config x` would otherwise silently ignore -config.
func takeDir(args []string, def string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return def, args
}

// splitList parses a comma-separated flag into a slice, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// signalContext cancels on SIGINT/SIGTERM so an interrupted suite still writes
// its report instead of losing what it already paid for.
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}

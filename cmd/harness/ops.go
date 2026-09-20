package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/100xteam-ai/foreman/internal/audit"
	"github.com/100xteam-ai/foreman/internal/deadletter"
)

// cmdReplay prints a human-readable timeline of a run's NDJSON event log
// (plan Step 17). It reads the configured sink, so it works for a file log, a
// spool file left behind by a crash, and an object in S3/MinIO.
func cmdReplay(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	file := fs.String("file", "", "read this NDJSON file instead of the run's stored log")
	verbose := fs.Bool("v", false, "show assistant text, thinking and token usage")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness replay <run_id> [-v]   |   harness replay -file <log.ndjson> [-v]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := audit.ReplayOptions{Verbose: *verbose}
	if *file != "" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		return audit.Replay(os.Stdout, f, opts)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("replay: one run id (or -file) is required")
	}
	runID := fs.Arg(0)
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, err := app.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	fmt.Printf("run %s  task %s  attempt %d  phase %d  status %s\n", run.ID, run.TaskID, run.Attempt, run.Phase, run.Status)
	if run.LastError != "" {
		fmt.Printf("last error: %s\n", run.LastError)
	}
	fmt.Printf("session %s (%s)  log %s\n\n", run.SessionID, run.SessionMode, run.EventLogURI)
	rc, err := app.HTTP.Audit.Reader(ctx, runID)
	if err != nil {
		return fmt.Errorf("no event log for run %s: %w", runID, err)
	}
	defer func() { _ = rc.Close() }()
	return audit.Replay(os.Stdout, rc, opts)
}

// cmdRequeue re-enqueues a dead run with a fresh attempt counter (plan Step 18).
func cmdRequeue(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	by := fs.String("by", "", "who requeued it (default $USER)")
	note := fs.String("note", "", "note for the worker, added to the failure evidence")
	process := fs.Bool("process", false, "process the requeued run to completion instead of leaving it queued")
	wait := fs.Duration("timeout", 30*time.Minute, "give up after this long (with -process)")
	list := fs.Bool("list", false, "list open dead letters instead of requeueing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness requeue <run_id> [-note text] [-process]   |   harness requeue -list")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*list && fs.NArg() != 1 {
		fs.Usage()
		return errors.New("requeue: one run id is required")
	}
	if *by == "" {
		if *by = os.Getenv("USER"); *by == "" {
			*by = "operator"
		}
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *list {
		entries, err := app.Store.ListOpen(ctx, 100)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("no open dead letters")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "RUN\tTASK\tKIND\tATTEMPT\tDEAD AT\tREASON"); err != nil {
			return err
		}
		for _, e := range entries {
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", e.RunID, e.TaskID, e.Kind, e.Attempt,
				e.DeadAt.Format(time.RFC3339), clipReason(e.Reason)); err != nil {
				return err
			}
		}
		return w.Flush()
	}

	runID := fs.Arg(0)
	next, err := app.Pool.Requeue(ctx, runID, *by, *note)
	if err != nil {
		if errors.Is(err, deadletter.ErrNotFound) {
			return fmt.Errorf("run %s has no dead-letter entry: %w", runID, err)
		}
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if !*process {
		fmt.Printf("requeued %s as %s (attempt %d, %s)\n", runID, next.ID, next.Attempt, next.SessionMode)
		return enc.Encode(next)
	}
	logger.Info("processing the requeued run", "task_id", next.TaskID, "run_id", next.ID)
	final, err := app.DriveTask(ctx, next.TaskID)
	if final != nil {
		_ = enc.Encode(final)
	}
	return err
}

// cmdBudget prints today's spend against every ceiling (plan Step 15).
func cmdBudget(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("budget", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	if app.Budget == nil {
		fmt.Println("no daily budgets configured (budgets.daily_usd)")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := app.Budget.Report(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "KEY\tSPENT USD\tLIMIT USD\tREMAINING\tRUNS\tSTATE"); err != nil {
		return err
	}
	for _, r := range rows {
		state := "ok"
		switch {
		case r.Exceeded():
			state = "EXHAUSTED"
		case r.Limit == 0:
			state = "no ceiling"
		}
		if _, err := fmt.Fprintf(w, "%s\t%.4f\t%.2f\t%.4f\t%d\t%s\n", r.Key, r.USD, r.Limit, r.Remaining(), r.Runs, state); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if app.Budget.BreakerOpen() {
		fmt.Println("\nglobal circuit breaker is OPEN: no new runs start until the UTC day rolls over")
	}
	return nil
}

func clipReason(s string) string {
	if len(s) <= 70 {
		return s
	}
	return s[:69] + "…"
}

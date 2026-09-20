// Command harness is the single binary: serve | run-once | doctor | version | eval.
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
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/runner"
)

const usage = `usage: harness <command> [flags]

commands:
  serve      run the HTTP intake, cron scheduler and worker pool
  run-once   submit one task from a JSON file and process it to completion
  review     approve | reject | close a run that waits for human review
  egress     run the worker egress allowlist proxy (worker.mode: docker)
  replay     print a run's event stream as a readable timeline
  tree       print a fan-out task, its children and every run as one tree
  requeue    re-enqueue a dead run with a fresh attempt counter (-list to see them)
  budget     show today's spend against the daily ceilings
  doctor     check Go, git, the pinned Claude Code CLI and credentials
  version    print harness and pinned CLI versions
  eval       run the golden suite, gate a change against a baseline, or list cases
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	loaded, derr := loadDotenv(".env")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)
	if derr != nil {
		logger.Warn("could not read .env", "err", derr)
	} else if len(loaded) > 0 {
		logger.Debug("loaded .env", "keys", loaded)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:], logger)
	case "run-once":
		err = cmdRunOnce(os.Args[2:], logger)
	case "review":
		err = cmdReview(os.Args[2:], logger)
	case "egress":
		err = cmdEgress(os.Args[2:], logger)
	case "replay":
		err = cmdReplay(os.Args[2:], logger)
	case "tree":
		err = cmdTree(os.Args[2:], logger)
	case "requeue":
		err = cmdRequeue(os.Args[2:], logger)
	case "budget":
		err = cmdBudget(os.Args[2:], logger)
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version":
		fmt.Printf("harness %s (pinned claude CLI %s)\n", version, runner.PinnedCLIVersion)
	case "eval":
		err = cmdEval(os.Args[2:], logger)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// version is set with -ldflags "-X main.version=…".
var version = "dev"

func logLevel() slog.Level {
	switch os.Getenv("HARNESS_LOG") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func cmdServe(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return app.Serve(ctx)
}

func cmdRunOnce(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("run-once", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	taskPath := fs.String("task", "", "task spec JSON file (same shape as POST /tasks)")
	keep := fs.Bool("keep-workspace", false, "do not destroy the workspace afterwards")
	wait := fs.Duration("timeout", 30*time.Minute, "give up after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskPath == "" {
		return errors.New("run-once: -task is required")
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	app.Pool.Config.KeepWorkspaces = *keep

	specBytes, err := os.ReadFile(*taskPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	run, err := app.RunOnce(ctx, specBytes)
	if run != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(run)
	}
	return err
}

// cmdReview applies a human decision from the terminal (the non-Slack path).
func cmdReview(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	runID := fs.String("run", "", "run id waiting in needs_review")
	by := fs.String("by", "", "who decides (default $USER)")
	comment := fs.String("comment", "", "note for the worker (reject) or the record")
	process := fs.Bool("process", false, "after the decision, process the task's follow-up runs to completion")
	wait := fs.Duration("timeout", 30*time.Minute, "give up after this long (with -process)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness review -run <run_id> [-by who] [-comment text] [-process] approve|reject|close")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runID == "" || fs.NArg() != 1 {
		fs.Usage()
		return errors.New("review: -run and exactly one action are required")
	}
	action := review.Action(fs.Arg(0))
	if !action.Valid() {
		return fmt.Errorf("review: unknown action %q (approve|reject|close)", fs.Arg(0))
	}
	if *by == "" {
		*by = os.Getenv("USER")
		if *by == "" {
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

	out, err := app.Pool.Decide(ctx, review.Decision{RunID: *runID, Action: action, By: *by, Comment: *comment, Source: "cli", At: time.Now()})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if !*process || out.NextRun == nil {
		return enc.Encode(out)
	}
	logger.Info("processing follow-up runs", "task_id", out.Run.TaskID, "next_run_id", out.NextRun.ID)
	final, err := app.DriveTask(ctx, out.Run.TaskID)
	if final != nil {
		_ = enc.Encode(final)
	}
	return err
}

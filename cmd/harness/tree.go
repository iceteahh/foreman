package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// cmdTree prints a fan-out task and its children as one tree (plan Step 21).
// A fan-out spreads a change over N+2 runs across N+1 task records, so
// `harness tree` is what an operator reads instead of stitching `GET /tasks`
// responses together by hand.
func cmdTree(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("tree", flag.ContinueOnError)
	cfgPath := fs.String("config", "harness.yaml", "path to harness.yaml")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: harness tree <task_id>   (a fan-out parent, or any child of one)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("tree: one task id is required")
	}
	app, err := buildApp(*cfgPath, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t, err := app.Store.GetTask(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	// Given a child, print the whole family: that is what the operator meant.
	if t.IsChild() {
		parent, perr := app.Store.GetTask(ctx, t.ParentID)
		if perr != nil && !errors.Is(perr, store.ErrNotFound) {
			return perr
		}
		if perr == nil {
			t = parent
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "TASK\tKIND\tPHASE\tRUN\tSTATUS\tCOST\tNOTE\n")
	if err := printTaskRows(ctx, app, w, t, ""); err != nil {
		return err
	}
	kids, err := app.Store.ListChildTasks(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if err := printTaskRows(ctx, app, w, k, "  └─ "); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(kids) > 0 {
		pending, err := app.Pool.FanOutPending(ctx, t.ID)
		if err != nil {
			return err
		}
		state := "fan-in done"
		if pending {
			state = "waiting for children"
		}
		fmt.Printf("\n%d child task(s), %s\n", len(kids), state)
	}
	return nil
}

func printTaskRows(ctx context.Context, app *app, w *tabwriter.Writer, t *task.Task, indent string) error {
	runs, err := app.Store.ListRunsByTask(ctx, t.ID)
	if err != nil {
		return err
	}
	label := indent + t.ID
	if title := strings.TrimSpace(t.Title); title != "" {
		label += " (" + truncate(title, 40) + ")"
	}
	if len(runs) == 0 {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t-\t-\t-\t(no runs yet)\n", label, t.Kind, t.Phase)
		return nil
	}
	for i, r := range runs {
		cell := label
		if i > 0 {
			cell = indent + strings.Repeat(" ", len(t.ID))
		}
		note := r.LastError
		if note == "" && len(r.Artifacts) > 0 {
			note = strings.Join(r.Artifacts, " ")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s (a%d)\t%s\t$%.4f\t%s\n",
			cell, t.Kind, r.Phase, r.ID, r.Attempt, r.Status, r.Metrics.CostUSD, truncate(note, 60))
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

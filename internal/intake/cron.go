package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/robfig/cron/v3"

	"github.com/100xteam-ai/foreman/internal/config"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Cron submits the tasks declared under `cron:` in harness.yaml.
type Cron struct {
	Submit Submitter
	// Gate decides whether this replica may fire an entry. With stateless
	// orchestrator replicas (plan Step 22) every replica holds the same
	// schedule, so an ungated firing would submit the task N times and spend
	// N budgets. nil fires every entry (one process).
	Gate    func(ctx context.Context, entry string) bool
	Logger  *slog.Logger
	entries []config.CronEntry
	c       *cron.Cron
}

// NewCron validates the schedules and task specs up front.
func NewCron(entries []config.CronEntry, submit Submitter, logger *slog.Logger) (*Cron, error) {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Cron{Submit: submit, Logger: logger, entries: entries, c: cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)))}
	for _, e := range entries {
		spec, err := SpecFromMap(e.Task)
		if err != nil {
			return nil, fmt.Errorf("cron %q: %w", e.Name, err)
		}
		if spec.RequestedBy == "" {
			spec.RequestedBy = "cron:" + e.Name
		}
		if spec.Title == "" {
			spec.Title = e.Name
		}
		name := e.Name
		if _, err := c.c.AddFunc(e.Schedule, func() { c.fire(name, spec) }); err != nil {
			return nil, fmt.Errorf("cron %q schedule %q: %w", e.Name, e.Schedule, err)
		}
	}
	return c, nil
}

// SpecFromMap converts a YAML task block into a task.Spec.
func SpecFromMap(m map[string]any) (task.Spec, error) {
	var spec task.Spec
	b, err := json.Marshal(m)
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return spec, fmt.Errorf("task block: %w", err)
	}
	if spec.Kind == "" || spec.Prompt == "" {
		return spec, fmt.Errorf("task block needs kind and prompt")
	}
	return spec, nil
}

func (c *Cron) fire(name string, spec task.Spec) {
	ctx := context.Background()
	if c.Gate != nil && !c.Gate(ctx, name) {
		c.Logger.Debug("cron firing handled by another replica", "cron", name)
		return
	}
	t, r, err := c.Submit.Submit(ctx, spec)
	if err != nil {
		c.Logger.Error("cron submit failed", "cron", name, "err", err)
		return
	}
	c.Logger.Info("cron task submitted", "cron", name, "task_id", t.ID, "run_id", r.ID)
}

// Start begins scheduling; Stop waits for running submissions.
func (c *Cron) Start() { c.c.Start() }

// Stop halts the scheduler.
func (c *Cron) Stop() context.Context { return c.c.Stop() }

// Len is the number of scheduled entries.
func (c *Cron) Len() int { return len(c.c.Entries()) }

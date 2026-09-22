package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/100xteam-ai/foreman/internal/fanout"
	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/templates"
)

// fanOut turns a passed planner run into one child task per subtask (design
// §6 fan-out, plan Step 21). Children are independent tasks of their own kind:
// each gets its own workspace, its own runs, its own retries and its own
// delivery, and the parent waits in its planner phase until every one of them
// is terminal. It returns the children it created.
//
// The parent's phase is *not* advanced here. The synthesizer is enqueued by
// the fan-in (maybeFanIn), which is the only place that knows the children
// have finished — and which one child wins the compare-and-swap that enqueues it.
func (p *Pool) fanOut(ctx context.Context, t *task.Task, planner *task.Run) ([]*task.Task, error) {
	if !t.FansOut() {
		return nil, fmt.Errorf("task %s does not fan out", t.ID)
	}
	if p.Submit == nil {
		return nil, errors.New("fan-out needs a submitter")
	}
	plan, err := fanout.Decode(planner.Output)
	if err != nil {
		return nil, err
	}
	specs, dropped, err := plan.Specs(t)
	if err != nil {
		return nil, err
	}
	if n := p.Config.MaxChildren; n > 0 && len(specs) > n {
		for _, s := range specs[n:] {
			dropped = append(dropped, s.Title)
		}
		specs = specs[:n]
	}
	if len(dropped) > 0 {
		// Dropped subtasks are announced, never silently discarded: the
		// synthesizer would otherwise report a complete change that is missing
		// whatever the planner listed last.
		p.log().Warn("fan-out capped; subtasks dropped", "task_id", t.ID, "kept", len(specs), "dropped", dropped)
	}

	fork := t.Child.SessionMode == task.SessionFork
	opts := SubmitOptions{}
	if fork {
		// Children fork the planner so they inherit what it learned about the
		// repository. The pointer is the id the harness minted for the planner
		// run, which is where the snapshot was written.
		opts = SubmitOptions{ForkFrom: planner.SessionID, ForkFromTask: t.ID}
		if planner.SessionURI == "" {
			p.log().Warn("planner left no transcript; children start cold", "task_id", t.ID, "run_id", planner.ID)
			opts = SubmitOptions{}
		}
	}
	created := make([]*task.Task, 0, len(specs))
	var artifacts []string
	for i, spec := range specs {
		ct, cr, err := p.Submit.SubmitWith(ctx, spec, opts)
		if err != nil {
			// Children already created keep running: they are real tasks with
			// real workspaces, and killing them would waste paid work. The
			// parent stays in its planner phase, so the fan-in reports the
			// short fan-out once they finish.
			return created, fmt.Errorf("fan-out child %d/%d (%s): %w", i+1, len(specs), spec.Title, err)
		}
		created = append(created, ct)
		artifacts = append(artifacts, "child:"+ct.ID)
		p.log().Info("fan-out child created", "parent_task_id", t.ID, "child_task_id", ct.ID,
			"child_run_id", cr.ID, "title", spec.Title, "session_mode", cr.SessionMode)
	}
	if err := p.Store.RecordArtifacts(ctx, planner.ID, "", "", artifacts); err != nil {
		return created, err
	}
	p.metrics().FanOut(ctx, string(t.Kind), len(created))
	return created, nil
}

// children loads a parent's fan-out children with their newest runs.
func (p *Pool) children(ctx context.Context, parentID string) ([]fanout.Child, error) {
	tasks, err := p.Store.ListChildTasks(ctx, parentID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tasks))
	for _, ct := range tasks {
		ids = append(ids, ct.ID)
	}
	// One query for every child, not one per child: the sweeper calls this for
	// each fan-out parent on every tick.
	latest, err := p.Store.LatestRuns(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]fanout.Child, 0, len(tasks))
	for _, ct := range tasks {
		// A child with no run row has not started; it counts as pending so the
		// fan-in waits rather than synthesising over a hole.
		out = append(out, fanout.Child{Task: ct, Run: latest[ct.ID]})
	}
	return out, nil
}

// maybeFanIn enqueues the parent's synthesizer once every child is terminal.
// It is called after each child run finishes and by the fan-in sweeper, and is
// safe to call from any number of replicas at once: the phase advance is a
// compare-and-swap, so exactly one caller enqueues the run.
func (p *Pool) maybeFanIn(ctx context.Context, parentID string) (*task.Run, error) {
	t, err := p.Store.GetTask(ctx, parentID)
	if err != nil {
		return nil, err
	}
	planPhase := t.FanOutPhase()
	if planPhase == 0 || t.Phase != planPhase || planPhase >= len(t.Phases) {
		return nil, nil // not a fan-out parent, already advanced, or nothing to synthesise
	}
	kids, err := p.children(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if !fanout.AllDone(kids) {
		return nil, nil
	}
	planner, err := p.plannerRun(ctx, parentID, planPhase)
	if err != nil {
		return nil, err
	}
	// The planner must have finished: a parent whose planner is still waiting
	// for a human has not approved this fan-out, and synthesising over it
	// would deliver a merge report for work nobody accepted.
	if planner == nil || !planner.Status.Terminal() {
		return nil, nil
	}
	prompt, err := p.synthPrompt(t, planner, kids)
	if err != nil {
		return nil, err
	}
	next, err := task.PhaseRun(t, planner, planPhase+1, prompt, p.now())
	if err != nil {
		return nil, err
	}
	won, err := p.Store.AdvancePhase(ctx, t.ID, planPhase, planPhase+1)
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, nil // another replica (or sibling) got there first
	}
	if _, err := p.enqueueNext(ctx, t, planner, next, "synthesize:"); err != nil {
		// The claim is taken but nothing is queued, which would leave the
		// parent stuck forever: no pending children to trigger a fan-in and a
		// phase the sweeper no longer looks at. Hand the claim back so the
		// next sweep tries again.
		if _, rerr := p.Store.AdvancePhase(ctx, t.ID, planPhase+1, planPhase); rerr != nil {
			p.log().Error("could not release the fan-in claim; the parent needs an operator",
				"task_id", t.ID, "phase", planPhase+1, "err", rerr)
		}
		return nil, err
	}
	p.log().Info("fan-in complete; synthesizer queued", "task_id", t.ID, "run_id", next.ID,
		"children", len(kids), "delivered", fanout.Delivered(kids))
	p.metrics().FanIn(ctx, string(t.Kind), len(kids), fanout.Delivered(kids))
	return next, nil
}

// plannerRun returns the newest run of the given phase, or nil. It is not
// simply LatestRun: a fan-in that claimed the phase and then failed to queue
// the synthesizer leaves a run row for the *next* phase behind, and taking
// that as the planner would make the retry impossible.
func (p *Pool) plannerRun(ctx context.Context, taskID string, phase int) (*task.Run, error) {
	runs, err := p.Store.ListRunsByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var out *task.Run
	for _, r := range runs {
		if r.Phase == phase && (out == nil || !r.CreatedAt.Before(out.CreatedAt)) {
			out = r
		}
	}
	return out, nil
}

// FanOutPending reports whether taskID is a fan-out parent whose children are
// still being worked on, so a caller driving one task to completion (run-once,
// the golden executor) keeps processing instead of stopping at the planner.
func (p *Pool) FanOutPending(ctx context.Context, taskID string) (bool, error) {
	t, err := p.Store.GetTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	planPhase := t.FanOutPhase()
	if planPhase == 0 || t.Phase != planPhase {
		return false, nil
	}
	latest, err := p.Store.LatestRun(ctx, taskID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	// A planner waiting for a human is not pending work the pool can finish.
	if !latest.Status.Terminal() {
		return false, nil
	}
	kids, err := p.children(ctx, taskID)
	if err != nil {
		return false, err
	}
	// A child stuck in needs_review is pending work for a *person*, not for
	// the pool: a caller draining the tree would otherwise spin until its
	// deadline instead of reporting that somebody has to decide.
	if fanout.AllSettled(kids) {
		if blocked := fanout.Blocked(kids); len(blocked) > 0 {
			p.log().Info("fan-out is waiting for human review", "task_id", taskID, "blocked_children", len(blocked))
		}
		return false, nil
	}
	return len(kids) > 0, nil
}

// synthPrompt renders the synthesizer phase prompt from the children's
// recorded outcomes. It never reads a child's transcript.
func (p *Pool) synthPrompt(t *task.Task, planner *task.Run, kids []fanout.Child) (string, error) {
	ph := t.PhaseSpec(t.FanOutPhase() + 1)
	if ph == nil {
		return "", fmt.Errorf("task %s has no synthesizer phase", t.ID)
	}
	plan := string(planner.Output)
	var pretty bytes.Buffer
	if len(planner.Output) > 0 && json.Indent(&pretty, planner.Output, "", "  ") == nil {
		plan = pretty.String()
	}
	return templates.RenderPhase(ph.Prompt, templates.PhaseData{
		Plan: plan, Task: t.Prompt, Children: fanout.Summaries(kids),
	})
}

// notifyParent runs the fan-in check for a finished child. Failures are logged
// rather than returned: the child's own run is already complete, and the
// sweeper retries the fan-in.
func (p *Pool) notifyParent(ctx context.Context, child *task.Task) {
	if child == nil || !child.IsChild() {
		return
	}
	if _, err := p.maybeFanIn(ctx, child.ParentID); err != nil {
		p.log().Error("fan-in check failed; the sweeper will retry",
			"parent_task_id", child.ParentID, "child_task_id", child.ID, "err", err)
	}
}

// fanOutFailed routes a planner whose output could not be decomposed. An
// undecomposable plan is not a crash: the run passed its checks, so the
// harness asks a human instead of quietly finishing a task that did nothing.
func (p *Pool) fanOutFailed(ctx context.Context, t *task.Task, planner *task.Run, cause error) error {
	reason := "fan-out failed: " + strings.TrimSpace(cause.Error())
	p.log().Error("fan-out failed; sending the planner to review", "task_id", t.ID, "run_id", planner.ID, "err", cause)
	if err := p.Store.UpdateRunStatus(ctx, planner.ID, task.StatusNeedsReview, reason); err != nil {
		return err
	}
	return p.postReview(ctx, t, planner, nil, reason)
}

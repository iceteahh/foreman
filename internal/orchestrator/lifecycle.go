package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/100xteam-ai/foreman/internal/deadletter"

	"github.com/100xteam-ai/foreman/internal/eval/feedback"
	"github.com/100xteam-ai/foreman/internal/obs"
	"github.com/100xteam-ai/foreman/internal/queue"
	"github.com/100xteam-ai/foreman/internal/review"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
	"github.com/100xteam-ai/foreman/templates"
)

// retry creates attempt+1 of prev and enqueues it (plan Step 11). When the
// session is resumable the new run continues it with fb as the prompt;
// otherwise it is a cold start with a fresh id and the original task prepended.
func (p *Pool) retry(ctx context.Context, t *task.Task, prev *task.Run, canResume bool, fb string) (*task.Run, error) {
	t, err := p.Store.GetTask(ctx, t.ID) // fresh resume pointer
	if err != nil {
		return nil, err
	}
	mode, prompt := task.SessionContinue, fb
	if !canResume || t.SessionID == "" {
		mode = task.SessionNew
		orig, err := p.originalPrompt(ctx, t, prev)
		if err != nil {
			return nil, err
		}
		prompt = feedback.Cold(orig, fb)
		p.log().Warn("session not resumable; cold retry", "task_id", t.ID, "prev_run_id", prev.ID)
	}
	next, err := task.NextRun(t, prev, mode, prompt, p.now())
	if err != nil {
		return nil, err
	}
	return p.enqueueNext(ctx, t, prev, next, "retry:")
}

// originalPrompt walks the retry chain back to the phase's first attempt and
// returns the prompt that run started with.
func (p *Pool) originalPrompt(ctx context.Context, t *task.Task, r *task.Run) (string, error) {
	cur := r
	for i := 0; cur.RetryOf != "" && cur.Attempt > 1 && i < 100; i++ {
		prev, err := p.Store.GetRun(ctx, cur.RetryOf)
		if err != nil {
			return "", err
		}
		if prev.Phase != cur.Phase {
			break
		}
		cur = prev
	}
	return task.Effective(t, cur).Prompt, nil
}

// advancePhase enqueues the first run of phase prev.Phase+1, resuming the task
// session with the phase prompt rendered from the approved output (design §6).
func (p *Pool) advancePhase(ctx context.Context, t *task.Task, prev *task.Run, comment string) (*task.Run, error) {
	t, err := p.Store.GetTask(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	n := prev.Phase + 1
	ph := t.PhaseSpec(n)
	if ph == nil {
		return nil, fmt.Errorf("task %s has no phase %d", t.ID, n)
	}
	plan := string(prev.Output)
	var pretty bytes.Buffer
	if len(prev.Output) > 0 && json.Indent(&pretty, prev.Output, "", "  ") == nil {
		plan = pretty.String()
	}
	prompt, err := templates.RenderPhase(ph.Prompt, templates.PhaseData{Plan: plan, Comment: comment, Task: t.Prompt})
	if err != nil {
		return nil, err
	}
	next, err := task.PhaseRun(t, prev, n, prompt, p.now())
	if err != nil {
		return nil, err
	}
	if err := p.Store.UpdateTaskPhase(ctx, t.ID, prev.Phase, n); err != nil {
		return nil, err
	}
	return p.enqueueNext(ctx, t, prev, next, "next:")
}

func (p *Pool) enqueueNext(ctx context.Context, t *task.Task, prev, next *task.Run, artifactPrefix string) (*task.Run, error) {
	if err := p.Store.CreateRun(ctx, next); err != nil {
		return nil, err
	}
	if _, err := p.Queue.Enqueue(ctx, queue.Job{RunID: next.ID, TaskID: t.ID, Kind: string(t.Kind), Priority: t.Priority}, 0); err != nil {
		return nil, err
	}
	if err := p.Store.RecordArtifacts(ctx, prev.ID, "", "", []string{artifactPrefix + next.ID}); err != nil {
		return nil, err
	}
	return next, nil
}

// Requeue re-enqueues a dead run with a fresh attempt counter (plan Step 18,
// `harness requeue <run_id>`). The dead run stays dead; the new run continues
// its session when a snapshot survives, carrying the failure evidence and the
// operator's note as feedback, and the dead-letter entry is marked requeued.
func (p *Pool) Requeue(ctx context.Context, runID, by, note string) (*task.Run, error) {
	p.init()
	prev, err := p.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if prev.Status != task.StatusDead {
		return nil, fmt.Errorf("run %s is %s, not dead: only dead runs are requeued", prev.ID, prev.Status)
	}
	t, err := p.Store.GetTask(ctx, prev.TaskID)
	if err != nil {
		return nil, err
	}
	if latest, err := p.Store.LatestRun(ctx, t.ID); err == nil && latest.ID != prev.ID && !latest.Status.Terminal() {
		return nil, fmt.Errorf("task %s already has a live run %s (%s)", t.ID, latest.ID, latest.Status)
	}
	// The requeued run inherits the dead run's phase, so requeuing a run from
	// a phase the task has already left would rewind it: the task would redo
	// planning after its plan was approved, and the approval would be lost.
	if prev.Phase != t.Phase {
		return nil, fmt.Errorf("run %s is from phase %d but task %s is in phase %d: requeuing it would rewind the task", prev.ID, prev.Phase, t.ID, t.Phase)
	}
	rep := review.Build(t, prev, nil, "").Checks
	fb := feedback.Render(prev.Attempt, rep, judgeFromRun(prev), note)
	mode, prompt := task.SessionContinue, fb
	if prev.SessionURI == "" || t.SessionID == "" {
		mode = task.SessionNew
		orig, err := p.originalPrompt(ctx, t, prev)
		if err != nil {
			return nil, err
		}
		prompt = feedback.Cold(orig, fb)
	}
	next, err := task.RequeueRun(t, prev, mode, prompt, p.now())
	if err != nil {
		return nil, err
	}
	if _, err := p.enqueueNext(ctx, t, prev, next, "requeue:"); err != nil {
		return nil, err
	}
	if p.DeadStore != nil {
		if err := p.DeadStore.MarkRequeued(ctx, prev.ID, next.ID, p.now()); err != nil && !errors.Is(err, deadletter.ErrNotFound) {
			p.log().Warn("marking the dead letter requeued failed", "run_id", prev.ID, "err", err)
		}
	}
	p.log().Info("dead run requeued", "run_id", prev.ID, "next_run_id", next.ID, "by", by, "session_mode", next.SessionMode)
	return next, nil
}

// postReview publishes a needs_review run to the channel and records the post.
func (p *Pool) postReview(ctx context.Context, t *task.Task, r *task.Run, capture *workspace.Capture, reason string) error {
	req := review.Build(t, r, capture, reason)
	ref, err := p.review().Post(ctx, req)
	if err != nil {
		return err
	}
	if p.Reviews == nil {
		return nil
	}
	return p.Reviews.SavePost(ctx, review.Post{RunID: r.ID, TaskID: t.ID, Channel: p.review().Name(), Ref: ref, PostedAt: p.now()})
}

// Decide implements review.Decider: a human's approve / reject / close on a
// needs_review run (design §3.2, §6).
func (p *Pool) Decide(ctx context.Context, d review.Decision) (*review.Outcome, error) {
	p.init()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.At.IsZero() {
		d.At = p.now()
	}
	r, err := p.Store.GetRun(ctx, d.RunID)
	if err != nil {
		return nil, err
	}
	if r.Status != task.StatusNeedsReview {
		return nil, fmt.Errorf("%w: run %s is %s", review.ErrNotReviewable, r.ID, r.Status)
	}
	t, err := p.Store.GetTask(ctx, r.TaskID)
	if err != nil {
		return nil, err
	}
	et := task.Effective(t, r)
	log := p.log().With("run_id", r.ID, "task_id", t.ID, "action", d.Action, "by", d.By)
	out := &review.Outcome{}
	who := d.By
	if d.Source != "" && d.Source != "slack" {
		who = d.Source + ":" + d.By
	}

	switch d.Action {
	case review.ActionApprove:
		switch {
		case t.FansOut() && r.Phase == t.FanOutPhase():
			// Approving a planner phase starts the parallel workers; the
			// synthesizer waits for the fan-in, not for this decision.
			kids, err := p.fanOut(ctx, t, r)
			if err != nil {
				return nil, fmt.Errorf("approve: fan out: %w", err)
			}
			msg := fmt.Sprintf("approved by %s; fanned out to %d child task(s)", who, len(kids))
			if err := p.Store.UpdateRunStatus(ctx, r.ID, task.StatusDelivered, msg); err != nil {
				return nil, err
			}
			out.Message = msg
			for _, c := range kids {
				out.Artifacts = append(out.Artifacts, "child:"+c.ID)
			}
			p.destroyKept(ctx, t, r)
			if _, err := p.maybeFanIn(ctx, t.ID); err != nil {
				log.Error("fan-in check after approval failed; the sweeper will retry", "err", err)
			}
		case t.HasPhases() && r.Phase < len(t.Phases):
			next, err := p.advancePhase(ctx, t, r, d.Comment)
			if err != nil {
				return nil, err
			}
			msg := fmt.Sprintf("approved by %s; phase %d queued as %s", who, next.Phase, next.ID)
			if err := p.Store.UpdateRunStatus(ctx, r.ID, task.StatusDelivered, msg); err != nil {
				return nil, err
			}
			out.NextRun, out.Message = next, msg
			p.destroyKept(ctx, t, r)
		default:
			artifacts, err := p.deliverKept(ctx, et, r)
			if err != nil {
				return nil, fmt.Errorf("approve: %w", err)
			}
			msg := fmt.Sprintf("approved by %s; delivered %v", who, artifacts)
			if err := p.Store.UpdateRunStatus(ctx, r.ID, task.StatusDelivered, msg); err != nil {
				return nil, err
			}
			out.Artifacts, out.Message = artifacts, msg
		}
	case review.ActionReject:
		rep, jv := review.Build(t, r, nil, "").Checks, judgeFromRun(r)
		fb := feedback.Render(r.Attempt, rep, jv, d.Comment)
		next, err := p.retry(ctx, t, r, r.SessionURI != "", fb)
		if err != nil {
			return nil, err
		}
		msg := fmt.Sprintf("rejected by %s; retry queued as %s (attempt %d)", who, next.ID, next.Attempt)
		if d.Comment != "" {
			msg += ": " + d.Comment
		}
		if err := p.Store.UpdateRunStatus(ctx, r.ID, task.StatusClosed, msg); err != nil {
			return nil, err
		}
		out.NextRun, out.Message = next, msg
		p.destroyKept(ctx, t, r)
	case review.ActionClose:
		msg := "closed by " + who
		if d.Comment != "" {
			msg += ": " + d.Comment
		}
		if err := p.Store.UpdateRunStatus(ctx, r.ID, task.StatusClosed, msg); err != nil {
			return nil, err
		}
		out.Message = msg
		p.destroyKept(ctx, t, r)
	}

	out.Run, err = p.Store.GetRun(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if p.Reviews != nil {
		rec := review.DecisionRecord{Decision: d, TaskID: t.ID, ChecksFailed: review.Build(t, r, nil, "").Checks.Failed()}
		if out.NextRun != nil {
			rec.NextRunID = out.NextRun.ID
		}
		if r.Eval != nil && r.Eval.Judge != nil {
			rec.JudgeVerdict = r.Eval.Judge.Verdict
		}
		if _, err := p.Reviews.RecordDecision(ctx, rec); err != nil {
			log.Error("recording review decision failed", "err", err)
		}
		if post, err := p.Reviews.GetPost(ctx, r.ID); err == nil {
			_ = p.Reviews.MarkResolved(ctx, r.ID, d.At)
			if err := p.review().Resolve(ctx, review.Build(t, out.Run, nil, ""), post.Ref, d, out); err != nil {
				log.Warn("updating review post failed", "err", err)
			}
		}
	}
	jv := ""
	if r.Eval != nil && r.Eval.Judge != nil {
		jv = r.Eval.Judge.Verdict
	}
	source := d.Source
	if source == "" {
		source = "api"
	}
	p.metrics().ReviewDecision(ctx, string(t.Kind), string(d.Action), source, jv)
	if obs.Disagrees(jv, string(d.Action)) {
		log.Warn("human overrode the judge", "judge_verdict", jv, "action", d.Action)
	}
	log.Info("review decision applied", "message", out.Message)
	p.finished(ctx, t, r.ID)
	return out, nil
}

// deliverKept reopens the kept workspace of a reviewed run and delivers it.
func (p *Pool) deliverKept(ctx context.Context, et *task.Task, r *task.Run) ([]string, error) {
	if p.Deliver == nil {
		return nil, errors.New("no delivery adapter configured")
	}
	ws, err := p.Workspaces.Open(ctx, et, r)
	if err != nil {
		return nil, err
	}
	rep := review.Build(et, r, nil, "").Checks
	in := deliverInput(et, r, ws, rep)
	artifacts, err := p.Deliver.Deliver(ctx, &in)
	if err != nil {
		return nil, err
	}
	if err := p.Store.RecordArtifacts(ctx, r.ID, "", "", artifacts); err != nil {
		return nil, err
	}
	if !p.Config.KeepWorkspaces {
		if err := ws.Destroy(); err != nil {
			p.log().Warn("destroy workspace after delivery", "err", err)
		}
	}
	return artifacts, nil
}

// destroyKept removes the workspace kept for review once no longer needed.
func (p *Pool) destroyKept(ctx context.Context, t *task.Task, r *task.Run) {
	if p.Config.KeepWorkspaces {
		return
	}
	ws, err := p.Workspaces.Open(ctx, t, r)
	if err != nil {
		return
	}
	if err := ws.Destroy(); err != nil {
		p.log().Warn("destroy kept workspace", "run_id", r.ID, "err", err)
	}
}

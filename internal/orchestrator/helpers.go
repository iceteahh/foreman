package orchestrator

import (
	"github.com/100xteam-ai/harness-loop-platform-go/internal/deliver"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/checks"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/eval/judge"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/workspace"
)

func deliverInput(et *task.Task, r *task.Run, ws workspace.Workspace, rep checks.Report) deliver.Input {
	return deliver.Input{Task: et, Run: r, Workspace: ws, Checks: rep}
}

// judgeFromRun rebuilds the judge verdict stored on a run (nil when Layer 2 did not run).
func judgeFromRun(r *task.Run) *judge.Verdict {
	if r.Eval == nil || r.Eval.Judge == nil {
		return nil
	}
	j := r.Eval.Judge
	return &judge.Verdict{Verdict: j.Verdict, Scores: j.Scores, GamedChecks: j.GamedChecks, Reasoning: j.Reasoning, CostUSD: j.CostUSD, Error: j.Error}
}

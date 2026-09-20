package review

import (
	"context"
	"log/slog"
	"strings"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// LogChannel is the fallback Channel when no Slack credentials are configured:
// requests are logged and answered through the HTTP API / CLI.
type LogChannel struct {
	Logger *slog.Logger
}

func (LogChannel) Name() string { return "log" }

func (l LogChannel) log() *slog.Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return slog.Default()
}

// Post implements Channel.
func (l LogChannel) Post(_ context.Context, req *Request) (Ref, error) {
	l.log().Warn("run needs human review", "run_id", req.Run.ID, "task_id", req.Task.ID, "title", req.Title(), "reason", req.Reason,
		"judge", judgeSummary(req.Judge), "checks", req.Checks.Summary(),
		"decide", "harness review -run "+req.Run.ID+" approve|reject|close  (or POST /runs/"+req.Run.ID+"/review)")
	return Ref{Channel: "log", ID: req.Run.ID}, nil
}

// Escalate implements Channel.
func (l LogChannel) Escalate(_ context.Context, req *Request, _ Ref) error {
	l.log().Error("review SLA expired; run still waiting", "run_id", req.Run.ID, "task_id", req.Task.ID, "title", req.Title())
	return nil
}

// Resolve implements Channel.
func (l LogChannel) Resolve(_ context.Context, req *Request, _ Ref, d Decision, out *Outcome) error {
	attrs := []any{"run_id", req.Run.ID, "action", d.Action, "by", d.By}
	if out != nil {
		attrs = append(attrs, "message", out.Message)
	}
	l.log().Info("review decision applied", attrs...)
	return nil
}

func judgeSummary(j *task.JudgeVerdict) string {
	if j == nil {
		return "skipped"
	}
	s := j.Verdict
	if j.GamedChecks {
		s += " (gamed checks)"
	}
	if r := strings.TrimSpace(j.Reasoning); r != "" {
		if len(r) > 200 {
			r = r[:199] + "…"
		}
		s += ": " + r
	}
	return s
}

var _ Channel = LogChannel{}

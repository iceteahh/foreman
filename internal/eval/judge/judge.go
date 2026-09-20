// Package judge is Layer 2 of the evaluation pipeline (design §5.2): a fresh,
// read-only, ephemeral `claude -p` scores the worker's result against a rubric
// with --json-schema. It sees the task spec, the diff, acceptance outputs and
// the check summary — never the worker's transcript. Judge failures are
// `uncertain`, never `pass`. This package and runner are the only ones that
// spawn the CLI.
package judge

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/runner"
	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/internal/workspace"
)

//go:embed rubric.json prompt.md.tmpl
var files embed.FS

// Rubric is the structured-output schema passed as --json-schema and used to
// re-validate the verdict independently of the CLI.
var Rubric = mustRead("rubric.json")

// ScoreNames are the rubric's numeric fields.
var ScoreNames = []string{"task_completion", "minimal_diff", "no_scope_creep", "code_quality"}

// Spawner runs one CLI invocation. *runner.Runner implements it; tests fake it.
type Spawner interface {
	Run(ctx context.Context, t *task.Task, r *task.Run, sp runner.Spawn) (*runner.Result, error)
}

// Options bound the judge (harness.yaml `judge:`).
type Options struct {
	// Model is used when policy.judge.model is empty; "" keeps the CLI default.
	Model string
	// MaxTurns defaults to 5, MaxCostUSD to 0.25, Timeout to 3 minutes.
	MaxTurns   int
	MaxCostUSD float64
	Timeout    time.Duration
	// AllowedTools defaults to ["Read"]; the judge is read-only by design.
	AllowedTools []string
	// Parallel runs samples concurrently (default sequential).
	Parallel bool
	// MaxDiffBytes bounds the diff in the prompt (default 60 KiB).
	MaxDiffBytes int
}

func (o Options) withDefaults() Options {
	if o.MaxTurns <= 0 {
		o.MaxTurns = 5
	}
	if o.MaxCostUSD <= 0 {
		o.MaxCostUSD = 0.25
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Minute
	}
	if len(o.AllowedTools) == 0 {
		o.AllowedTools = []string{"Read"}
	}
	if o.MaxDiffBytes <= 0 {
		o.MaxDiffBytes = 60 << 10
	}
	return o
}

// Judge evaluates runs.
type Judge struct {
	Spawner Spawner
	Options Options
	Logger  *slog.Logger
}

// Input is what the judge may look at.
type Input struct {
	Task    *task.Task
	Run     *task.Run
	Checks  checks.Report
	Capture *workspace.Capture
	// Outputs are the acceptance command results keyed by command.
	Outputs map[string]workspace.ExecResult
	// Result is the worker's runner result; only its final text and
	// structured_output are shown to the judge, never the event stream.
	Result *runner.Result
	// Workspace is the checkout the judge may read; ConfigDir is the run's
	// config dir, from which per-sample dirs are derived.
	Workspace string
	ConfigDir string
}

// Sample is one judge invocation.
type Sample struct {
	Verdict     string         `json:"verdict"`
	Scores      map[string]int `json:"scores,omitempty"`
	GamedChecks bool           `json:"gamed_checks"`
	Reasoning   string         `json:"reasoning,omitempty"`
	CostUSD     float64        `json:"cost_usd"`
	Outcome     events.Outcome `json:"outcome,omitempty"`
	// Error explains why the sample is invalid (→ counted as uncertain).
	Error       string `json:"error,omitempty"`
	EventLogURI string `json:"event_log_uri,omitempty"`
}

// Valid reports whether the sample produced a rubric-conforming verdict.
func (s Sample) Valid() bool { return s.Error == "" }

// Verdict is the aggregated Layer 2 output.
type Verdict struct {
	Verdict     string         `json:"verdict"`
	Scores      map[string]int `json:"scores,omitempty"`
	GamedChecks bool           `json:"gamed_checks"`
	Reasoning   string         `json:"reasoning,omitempty"`
	Samples     []Sample       `json:"samples"`
	CostUSD     float64        `json:"cost_usd"`
	// Error is set when no sample was valid.
	Error string `json:"error,omitempty"`
}

// MinScore returns the lowest score and whether any score exists.
func (v *Verdict) MinScore() (int, bool) {
	if v == nil || len(v.Scores) == 0 {
		return 0, false
	}
	min, first := 0, true
	for _, s := range v.Scores {
		if first || s < min {
			min, first = s, false
		}
	}
	return min, true
}

// Failed reports whether the verdict blocks delivery (design §5.3 first branch).
func (v *Verdict) Failed() bool {
	return v != nil && (v.Verdict == task.VerdictFail || v.GamedChecks)
}

// ToTask converts to the stored shape.
func (v *Verdict) ToTask() *task.JudgeVerdict {
	if v == nil {
		return nil
	}
	return &task.JudgeVerdict{Verdict: v.Verdict, Scores: v.Scores, GamedChecks: v.GamedChecks, Reasoning: v.Reasoning,
		Samples: len(v.Samples), CostUSD: v.CostUSD, Error: v.Error}
}

// Uncertain builds the verdict for a judge that could not run.
func Uncertain(reason string) *Verdict {
	return &Verdict{Verdict: task.VerdictUncertain, Reasoning: "judge unavailable: " + reason, Error: reason}
}

// Evaluate runs policy.judge.samples judges and aggregates them. It returns an
// error only for programming mistakes (nil input); judge failures come back as
// an uncertain verdict so the router can send the run to a human.
func (j *Judge) Evaluate(ctx context.Context, in *Input) (*Verdict, error) {
	if in == nil || in.Task == nil || in.Run == nil {
		return nil, errors.New("judge: task and run are required")
	}
	if j.Spawner == nil {
		return nil, errors.New("judge: Spawner is required")
	}
	if in.Workspace == "" || in.ConfigDir == "" {
		return nil, errors.New("judge: Workspace and ConfigDir are required")
	}
	opts := j.Options.withDefaults()
	prompt, err := Prompt(in, opts.MaxDiffBytes)
	if err != nil {
		return nil, err
	}
	n := in.Task.Policy.Judge.Samples
	if n <= 0 {
		n = 1
	}
	samples := make([]Sample, n)
	run := func(i int) { samples[i] = j.sample(ctx, in, opts, prompt, i+1) }
	if opts.Parallel && n > 1 {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); run(i) }(i)
		}
		wg.Wait()
	} else {
		for i := 0; i < n; i++ {
			run(i)
		}
	}
	v := Aggregate(samples)
	j.logger().Info("judge verdict", "run_id", in.Run.ID, "verdict", v.Verdict, "gamed", v.GamedChecks, "scores", v.Scores, "samples", n, "cost_usd", v.CostUSD, "error", v.Error)
	return v, nil
}

// judgeTask builds the ephemeral judge invocation as a task + run so the
// runner applies the same isolation (env allowlist, process group, timeout,
// budget backstop, audit) as it does for workers.
func (j *Judge) judgeTask(in *Input, opts Options, prompt string, sample int) (*task.Task, *task.Run) {
	model := in.Task.Policy.Judge.Model
	if model == "" {
		model = opts.Model
	}
	jt := &task.Task{
		ID: in.Task.ID, Kind: in.Task.Kind, Title: "judge: " + in.Task.Title, Prompt: prompt,
		Workspace: in.Task.Workspace,
		Policy: task.Policy{AllowedTools: append([]string(nil), opts.AllowedTools...), MaxTurns: opts.MaxTurns,
			TimeoutMS: opts.Timeout.Milliseconds(), MaxCostUSD: opts.MaxCostUSD, Model: model},
		Acceptance:  task.Acceptance{JSONSchema: Rubric},
		RequestedBy: "judge",
		CreatedAt:   in.Task.CreatedAt,
	}
	jr := &task.Run{
		ID: fmt.Sprintf("%s-judge-%d", in.Run.ID, sample), TaskID: in.Task.ID, Attempt: sample,
		SessionID: task.NewSessionID(), SessionMode: task.SessionEphemeral, Status: task.StatusRunning,
		Artifacts: []string{}, CreatedAt: time.Now(),
	}
	return jt, jr
}

func (j *Judge) sample(ctx context.Context, in *Input, opts Options, prompt string, n int) Sample {
	jt, jr := j.judgeTask(in, opts, prompt, n)
	sp := runner.Spawn{Workspace: in.Workspace, ConfigDir: fmt.Sprintf("%s-judge-%d", in.ConfigDir, n)}
	res, err := j.Spawner.Run(ctx, jt, jr, sp)
	if err != nil && res == nil {
		return Sample{Verdict: task.VerdictUncertain, Error: "spawn: " + err.Error()}
	}
	s := ParseResult(res)
	if err != nil && s.Error == "" {
		s.Error = err.Error()
	}
	return s
}

// ParseResult turns a runner result into a Sample, validating the structured
// output against the rubric independently of the CLI.
func ParseResult(res *runner.Result) Sample {
	s := Sample{Verdict: task.VerdictUncertain}
	if res == nil {
		s.Error = "no result"
		return s
	}
	s.EventLogURI = res.EventLogURI
	s.Outcome = res.Outcome
	if res.Result != nil {
		s.CostUSD = res.Result.TotalCostUSD
	}
	switch {
	case res.Killed != runner.KillNone:
		s.Error = "judge killed: " + string(res.Killed)
		return s
	case res.Result == nil:
		s.Error = "judge exited without a result event (" + string(res.Outcome) + ")"
		return s
	case res.Outcome != events.OutcomeCompleted || res.Result.IsError:
		s.Error = fmt.Sprintf("judge did not complete: outcome=%s is_error=%v %s", res.Outcome, res.Result.IsError, strings.Join(res.Result.Errors, "; "))
		return s
	}
	out := res.Result.StructuredOutput
	if len(strings.TrimSpace(string(out))) == 0 || strings.TrimSpace(string(out)) == "null" {
		// Plain runs put the text in result.result; accept it when it is the JSON.
		if txt := strings.TrimSpace(res.Result.Text()); strings.HasPrefix(txt, "{") {
			out = json.RawMessage(txt)
		} else {
			s.Error = "judge produced no structured_output"
			return s
		}
	}
	if err := checks.ValidateJSON(Rubric, out); err != nil {
		s.Error = "judge output does not match the rubric: " + err.Error()
		return s
	}
	var raw struct {
		Verdict     string `json:"verdict"`
		GamedChecks bool   `json:"gamed_checks"`
		Reasoning   string `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		s.Error = "judge output is not JSON: " + err.Error()
		return s
	}
	var scores map[string]any
	_ = json.Unmarshal(out, &scores)
	s.Scores = map[string]int{}
	for _, name := range ScoreNames {
		if v, ok := scores[name].(float64); ok {
			s.Scores[name] = int(v)
		}
	}
	s.Verdict, s.GamedChecks, s.Reasoning = raw.Verdict, raw.GamedChecks, strings.TrimSpace(raw.Reasoning)
	if s.GamedChecks {
		s.Verdict = task.VerdictFail // automatic fail regardless of scores (§5.2)
	}
	return s
}

// Aggregate applies majority voting over valid samples: the modal verdict
// wins, ties are uncertain, gamed_checks needs a majority (one sample when
// samples == 1), scores are averaged and rounded. No valid sample → uncertain.
func Aggregate(samples []Sample) *Verdict {
	v := &Verdict{Samples: samples, Scores: map[string]int{}}
	votes := map[string]int{}
	gamed, valid := 0, 0
	sums, counts := map[string]int{}, map[string]int{}
	var reasons, errs []string
	for i, s := range samples {
		v.CostUSD += s.CostUSD
		if !s.Valid() {
			errs = append(errs, fmt.Sprintf("sample %d: %s", i+1, s.Error))
			continue
		}
		valid++
		votes[s.Verdict]++
		if s.GamedChecks {
			gamed++
		}
		for k, n := range s.Scores {
			sums[k] += n
			counts[k]++
		}
		if s.Reasoning != "" {
			if len(samples) > 1 {
				reasons = append(reasons, fmt.Sprintf("Sample %d (%s): %s", i+1, s.Verdict, s.Reasoning))
			} else {
				reasons = append(reasons, s.Reasoning)
			}
		}
	}
	if valid == 0 {
		v.Verdict = task.VerdictUncertain
		v.Error = strings.Join(errs, "; ")
		v.Reasoning = "judge unavailable: " + v.Error
		v.Scores = nil
		return v
	}
	best, bestN, tie := "", 0, false
	for _, name := range []string{task.VerdictPass, task.VerdictFail, task.VerdictUncertain} {
		switch n := votes[name]; {
		case n > bestN:
			best, bestN, tie = name, n, false
		case n == bestN && n > 0:
			tie = true
		}
	}
	if tie {
		best = task.VerdictUncertain
	}
	v.Verdict = best
	v.GamedChecks = gamed*2 > valid
	if v.GamedChecks {
		v.Verdict = task.VerdictFail
	}
	for k, sum := range sums {
		v.Scores[k] = (sum + counts[k]/2) / counts[k]
	}
	if len(v.Scores) == 0 {
		v.Scores = nil
	}
	if len(errs) > 0 {
		reasons = append(reasons, "Invalid samples: "+strings.Join(errs, "; "))
	}
	v.Reasoning = strings.Join(reasons, "\n\n")
	return v
}

func (j *Judge) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}

func mustRead(name string) json.RawMessage {
	b, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// sortedKeys is used by the prompt renderer for stable output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ Spawner = (*runner.Runner)(nil)

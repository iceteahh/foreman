package golden

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/task"
)

// Execution is what an Executor reports back about one case. Runs holds every
// attempt of the task in creation order, so the suite can measure the retry
// rate and the cost of the whole task, not just of the last run.
type Execution struct {
	Task         *task.Task
	Runs         []*task.Run
	Final        *task.Run
	ChangedFiles []string
	// DurationMS is wall-clock time for the whole case, including provisioning
	// and evaluation. It is the number an operator feels, unlike run.metrics.
	DurationMS int64
	// Err is a harness failure (provisioning, spawn, timeout). It scores as
	// OutcomeError, which never matches an expectation.
	Err error
}

// CaseResult is one row of the report.
type CaseResult struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Description  string   `json:"description,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Expected     Outcome  `json:"expected"`
	Actual       Outcome  `json:"actual"`
	OK           bool     `json:"ok"`
	Skipped      bool     `json:"skipped,omitempty"`
	SkipReason   string   `json:"skip_reason,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
	TaskID       string   `json:"task_id,omitempty"`
	RunID        string   `json:"run_id,omitempty"`
	Status       string   `json:"status,omitempty"`
	Attempts     int      `json:"attempts,omitempty"`
	Turns        int      `json:"turns,omitempty"`
	CostUSD      float64  `json:"cost_usd,omitempty"`
	DurationMS   int64    `json:"duration_ms,omitempty"`
	Model        string   `json:"model,omitempty"`
	JudgeVerdict string   `json:"judge_verdict,omitempty"`
	// JudgeAgreed is set only when the case is judged and declares an
	// outcome: it records whether Layer 2 and the human-blessed expectation
	// pointed the same way (design §5.4 "judge/human agreement rate").
	JudgeAgreed *bool `json:"judge_agreed,omitempty"`
	// Samples and Passes are set when the case ran more than once
	// (`eval run -repeat N`): OK is then the majority verdict, and Passes/Samples
	// is the observed pass rate for this one case. A worker is not a
	// deterministic function, so one sample cannot tell a flaky case from a
	// regression.
	Samples      int      `json:"samples,omitempty"`
	Passes       int      `json:"passes,omitempty"`
	ChecksFailed []string `json:"checks_failed,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// Skipped renders a case that was not executed.
func skipped(c *Case, reason string) CaseResult {
	return CaseResult{ID: c.ID, Kind: string(c.Kind), Description: c.Description, Tags: c.Tags,
		Expected: c.Expect.Outcome, Actual: "", OK: true, Skipped: true, SkipReason: reason}
}

// Score compares an execution against the case's expectations. It is pure: the
// suite runner is responsible for producing ex, this decides what it means.
func Score(c *Case, ex *Execution) CaseResult {
	res := CaseResult{ID: c.ID, Kind: string(c.Kind), Description: c.Description, Tags: c.Tags,
		Expected: c.Expect.Outcome, Actual: OutcomeOf(ex.Final), ChangedFiles: ex.ChangedFiles, DurationMS: ex.DurationMS}
	if ex.Err != nil {
		res.Error = ex.Err.Error()
	}
	if ex.Task != nil {
		res.TaskID = ex.Task.ID
		res.Model = ex.Task.Policy.Model
	}
	if ex.Final != nil {
		res.RunID = ex.Final.ID
		res.Status = string(ex.Final.Status)
		res.Turns = totalTurns(ex.Runs)
		res.Attempts = len(ex.Runs)
		if res.Attempts == 0 {
			res.Attempts = ex.Final.Attempt
		}
		if ex.Final.Eval != nil {
			res.ChecksFailed = checks.FromOutcomes(ex.Final.Eval.Checks).FailedNames()
			if j := ex.Final.Eval.Judge; j != nil {
				res.JudgeVerdict = j.Verdict
			}
		}
	}
	res.CostUSD = TotalCost(ex.Runs)

	var reasons []string
	if ex.Err != nil {
		reasons = append(reasons, "harness error: "+ex.Err.Error())
	}
	if res.Actual != c.Expect.Outcome {
		reasons = append(reasons, fmt.Sprintf("expected %s, got %s (run status %s)", c.Expect.Outcome, res.Actual, res.Status))
		if ex.Final != nil && ex.Final.LastError != "" {
			reasons = append(reasons, "last error: "+firstLine(ex.Final.LastError))
		}
	}
	if n := c.Expect.MaxAttempts; n > 0 && res.Attempts > n {
		reasons = append(reasons, fmt.Sprintf("took %d attempts, expected at most %d", res.Attempts, n))
	}
	if v := c.Expect.JudgeVerdict; v != "" && v != res.JudgeVerdict {
		reasons = append(reasons, fmt.Sprintf("expected judge verdict %q, got %q", v, res.JudgeVerdict))
	}
	reasons = append(reasons, scoreFiles(c, ex.ChangedFiles)...)
	reasons = append(reasons, scoreOutput(c, ex.Final)...)

	if agreed, ok := judgeAgreement(c, res.JudgeVerdict); ok {
		res.JudgeAgreed = &agreed
	}
	res.Reasons = reasons
	res.OK = len(reasons) == 0
	return res
}

// scoreFiles checks the changed-file expectations. Only a pass outcome
// declares them (Case.Validate enforces that), because a failed run's diff is
// whatever the worker left behind.
func scoreFiles(c *Case, changed []string) []string {
	var reasons []string
	for _, g := range c.Expect.FilesTouched {
		if !matchAny(g, changed) {
			reasons = append(reasons, fmt.Sprintf("no changed file matches files_touched %q (changed: %s)", g, list(changed)))
		}
	}
	for _, g := range c.Expect.FilesUntouched {
		var hit []string
		for _, f := range changed {
			if matchGlob(g, f) {
				hit = append(hit, f)
			}
		}
		if len(hit) > 0 {
			reasons = append(reasons, fmt.Sprintf("files_untouched %q matched %s", g, list(hit)))
		}
	}
	return reasons
}

// scoreOutput validates the run's structured output against the case schema.
// It is the deliverable of the read-only kinds, so an absent output is a
// failure rather than a skip.
func scoreOutput(c *Case, r *task.Run) []string {
	if len(c.Expect.OutputSchema) == 0 {
		return nil
	}
	if r == nil || len(r.Output) == 0 {
		return []string{"expect.output_schema is set but the run produced no structured_output"}
	}
	if err := checks.ValidateJSON(c.Expect.OutputSchema, r.Output); err != nil {
		return []string{"structured_output does not match expect.output_schema: " + firstLine(err.Error())}
	}
	return nil
}

// judgeAgreement reports whether Layer 2 pointed the same way as the
// human-blessed expectation. A pass expectation agrees with a `pass` verdict;
// anything else agrees with `fail`. `uncertain` never agrees: it is the
// verdict that costs a human their time.
func judgeAgreement(c *Case, verdict string) (bool, bool) {
	if verdict == "" {
		return false, false
	}
	switch c.Expect.Outcome {
	case OutcomePass:
		return verdict == task.VerdictPass, true
	case OutcomeFail:
		return verdict == task.VerdictFail, true
	case OutcomeNeedsReview:
		// needs_review is exactly what `uncertain` is for.
		return verdict == task.VerdictUncertain, true
	default:
		return false, false
	}
}

// TotalCost sums the worker and judge spend of every attempt. The sum is
// rounded to the cent's sixth decimal: an unrounded float accumulates a
// different tail depending on the order the attempts are added, which would
// make two identical suite runs produce textually different reports.
func TotalCost(runs []*task.Run) float64 {
	var sum float64
	for _, r := range runs {
		sum += r.Metrics.CostUSD
		if r.Eval != nil && r.Eval.Judge != nil {
			sum += r.Eval.Judge.CostUSD
		}
	}
	return round(sum, 6)
}

func totalTurns(runs []*task.Run) int {
	n := 0
	for _, r := range runs {
		n += r.Metrics.Turns
	}
	return n
}

func matchGlob(g, file string) bool {
	file = path.Clean(strings.ReplaceAll(file, "\\", "/"))
	ok, err := doublestar.Match(g, file)
	return err == nil && ok
}

func matchAny(g string, files []string) bool {
	for _, f := range files {
		if matchGlob(g, f) {
			return true
		}
	}
	return false
}

func list(files []string) string {
	if len(files) == 0 {
		return "none"
	}
	out := append([]string(nil), files...)
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 300 {
		s = s[:299] + "…"
	}
	return s
}

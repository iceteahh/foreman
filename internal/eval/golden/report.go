package golden

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Totals are the design §5.4 suite metrics: pass rate, retry rate, average
// turns, cost and duration, and the judge/human agreement rate.
type Totals struct {
	Cases   int `json:"cases"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Errors counts cases the harness could not finish (a subset of Failed).
	Errors int `json:"errors"`
	// Scored is Cases minus Skipped: the denominator of every rate below.
	Scored int `json:"scored"`

	PassRate  float64 `json:"pass_rate"`
	RetryRate float64 `json:"retry_rate"`

	AvgTurns      float64 `json:"avg_turns"`
	AvgCostUSD    float64 `json:"avg_cost_usd"`
	AvgDurationMS float64 `json:"avg_duration_ms"`
	CostUSD       float64 `json:"cost_usd"`

	// JudgeAgreement is the fraction of judged cases where Layer 2 agreed with
	// the case's known-good outcome; JudgeCases is its denominator.
	JudgeAgreement float64 `json:"judge_agreement"`
	JudgeCases     int     `json:"judge_cases"`
}

// Report is one suite run, written to evals/reports/<date>.json.
type Report struct {
	Suite          string    `json:"suite"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	HarnessVersion string    `json:"harness_version,omitempty"`
	CLIVersion     string    `json:"cli_version,omitempty"`
	WorkerMode     string    `json:"worker_mode,omitempty"`
	// BudgetExhausted records that the suite stopped early on its cost cap, so
	// the pass rate covers only the cases that ran and must not gate a change.
	BudgetExhausted bool              `json:"budget_exhausted,omitempty"`
	MaxCostUSD      float64           `json:"max_cost_usd,omitempty"`
	Totals          Totals            `json:"totals"`
	ByKind          map[string]Totals `json:"by_kind,omitempty"`
	Cases           []CaseResult      `json:"cases"`
}

// Summarize fills Totals and ByKind from the case rows.
func (r *Report) Summarize() {
	r.Totals = totalsOf(r.Cases)
	byKind := map[string][]CaseResult{}
	for _, c := range r.Cases {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	r.ByKind = make(map[string]Totals, len(byKind))
	for k, rows := range byKind {
		r.ByKind[k] = totalsOf(rows)
	}
}

func totalsOf(rows []CaseResult) Totals {
	var t Totals
	var turns, cost, dur float64
	var agreed int
	for _, c := range rows {
		t.Cases++
		if c.Skipped {
			t.Skipped++
			continue
		}
		t.Scored++
		if c.OK {
			t.Passed++
		} else {
			t.Failed++
		}
		if c.Actual == OutcomeError {
			t.Errors++
		}
		if c.Attempts > 1 {
			t.RetryRate++
		}
		turns += float64(c.Turns)
		cost += c.CostUSD
		dur += float64(c.DurationMS)
		if c.JudgeAgreed != nil {
			t.JudgeCases++
			if *c.JudgeAgreed {
				agreed++
			}
		}
	}
	t.CostUSD = round(cost, 6)
	if t.Scored > 0 {
		n := float64(t.Scored)
		t.PassRate = round(float64(t.Passed)/n, 4)
		t.RetryRate = round(t.RetryRate/n, 4)
		t.AvgTurns = round(turns/n, 2)
		t.AvgCostUSD = round(cost/n, 6)
		t.AvgDurationMS = round(dur/n, 0)
	}
	if t.JudgeCases > 0 {
		t.JudgeAgreement = round(float64(agreed)/float64(t.JudgeCases), 4)
	}
	return t
}

// round keeps the report readable and, more importantly, stable: an unrounded
// float would make two identical suite runs produce textually different JSON.
func round(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

// Write saves the report as pretty JSON, creating the directory.
func (r *Report) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ReadReport loads a report written by Write.
func ReadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &rep, nil
}

// DefaultReportPath is evals/reports/<date>.json, the nightly CI artifact.
func DefaultReportPath(dir string, at time.Time) string {
	return filepath.Join(dir, at.UTC().Format("2006-01-02")+".json")
}

// Case returns the row for id.
func (r *Report) Case(id string) (CaseResult, bool) {
	for _, c := range r.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return CaseResult{}, false
}

// Render writes a human-readable summary: one line per case, then the totals.
func (r *Report) Render(w io.Writer) {
	out := &lines{w: w}
	rows := append([]CaseResult(nil), r.Cases...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, c := range rows {
		switch {
		case c.Skipped:
			out.printf("  SKIP  %-32s %s\n", c.ID, c.SkipReason)
		case c.OK:
			out.printf("  ok    %-32s %-13s %d attempt(s)  %d turns  $%.4f  %s\n",
				c.ID, c.Actual, c.Attempts, c.Turns, c.CostUSD, dur(c.DurationMS))
		default:
			out.printf("  FAIL  %-32s %-13s %d attempt(s)  %d turns  $%.4f  %s\n",
				c.ID, c.Actual, c.Attempts, c.Turns, c.CostUSD, dur(c.DurationMS))
			for _, reason := range c.Reasons {
				out.printf("          - %s\n", reason)
			}
		}
	}
	t := r.Totals
	out.printf("\n%d/%d passed (%.0f%%)", t.Passed, t.Scored, t.PassRate*100)
	if t.Skipped > 0 {
		out.printf(", %d skipped", t.Skipped)
	}
	out.printf(" \u00b7 retry rate %.0f%% \u00b7 avg %.1f turns, $%.4f, %s \u00b7 total $%.4f",
		t.RetryRate*100, t.AvgTurns, t.AvgCostUSD, dur(int64(t.AvgDurationMS)), t.CostUSD)
	if t.JudgeCases > 0 {
		out.printf(" \u00b7 judge agreement %.0f%% (%d judged)", t.JudgeAgreement*100, t.JudgeCases)
	}
	out.nl()
	if r.BudgetExhausted {
		out.printf("\nSTOPPED EARLY: the suite hit its $%.2f cost cap; this report must not gate a change.\n", r.MaxCostUSD)
	}
	if len(r.ByKind) > 1 {
		out.printf("\nby kind:\n")
		kinds := make([]string, 0, len(r.ByKind))
		for k := range r.ByKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			kt := r.ByKind[k]
			out.printf("  %-20s %d/%d  $%.4f\n", k, kt.Passed, kt.Scored, kt.CostUSD)
		}
	}
}

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

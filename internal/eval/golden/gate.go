package golden

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Limits are the regression gate's thresholds (design §5.4: "golden-suite pass
// rate may not drop more than a configured delta on any config change").
type Limits struct {
	// MaxPassRateDrop is how much the pass rate may fall below the baseline,
	// as a fraction (0.05 = five percentage points). Negative is rejected.
	MaxPassRateDrop float64
	// MinPassRate is an absolute floor, independent of the baseline; 0 disables it.
	MinPassRate float64
	// AllowNewFailures permits a case that passed in the baseline to fail now,
	// as long as the aggregate rate holds. Off by default: a swap of one
	// passing case for another hides a real regression behind a stable rate.
	AllowNewFailures bool
	// AllowMissingCases permits the report to omit baseline cases. Off by
	// default, so deleting a failing case cannot be used to pass the gate.
	AllowMissingCases bool
}

// DefaultLimits is the gate a PR must clear: five points of pass rate, no case
// that used to pass may start failing, and no case may quietly disappear.
func DefaultLimits() Limits { return Limits{MaxPassRateDrop: 0.05} }

// GateResult is the gate's verdict, with everything needed to explain it.
type GateResult struct {
	OK               bool     `json:"ok"`
	BaselinePassRate float64  `json:"baseline_pass_rate"`
	PassRate         float64  `json:"pass_rate"`
	Drop             float64  `json:"drop"`
	MaxDrop          float64  `json:"max_drop"`
	NewFailures      []string `json:"new_failures,omitempty"`
	Fixed            []string `json:"fixed,omitempty"`
	Missing          []string `json:"missing,omitempty"`
	Added            []string `json:"added,omitempty"`
	Reasons          []string `json:"reasons,omitempty"`
}

// Gate compares a suite run against a baseline. A report that stopped on its
// cost cap never passes: its pass rate describes a different set of cases.
func Gate(baseline, current *Report, lim Limits) GateResult {
	res := GateResult{MaxDrop: lim.MaxPassRateDrop, PassRate: current.Totals.PassRate}
	if baseline != nil {
		res.BaselinePassRate = baseline.Totals.PassRate
		res.Drop = round(baseline.Totals.PassRate-current.Totals.PassRate, 4)
	}
	if current.BudgetExhausted {
		res.Reasons = append(res.Reasons, fmt.Sprintf("the suite stopped on its $%.2f cost cap, so its pass rate covers only part of the suite", current.MaxCostUSD))
	}
	if current.Totals.Scored == 0 {
		res.Reasons = append(res.Reasons, "no case was scored (every case skipped or the suite is empty)")
	}
	if res.Drop > lim.MaxPassRateDrop {
		res.Reasons = append(res.Reasons, fmt.Sprintf("pass rate fell %.1f points (%.0f%% → %.0f%%), more than the allowed %.1f",
			res.Drop*100, res.BaselinePassRate*100, res.PassRate*100, lim.MaxPassRateDrop*100))
	}
	if lim.MinPassRate > 0 && current.Totals.PassRate < lim.MinPassRate {
		res.Reasons = append(res.Reasons, fmt.Sprintf("pass rate %.0f%% is below the floor of %.0f%%", current.Totals.PassRate*100, lim.MinPassRate*100))
	}
	if baseline != nil {
		res.NewFailures, res.Fixed, res.Missing, res.Added = diffCases(baseline, current)
		if len(res.NewFailures) > 0 && !lim.AllowNewFailures {
			res.Reasons = append(res.Reasons, "case(s) that passed in the baseline now fail: "+strings.Join(res.NewFailures, ", "))
		}
		if len(res.Missing) > 0 && !lim.AllowMissingCases {
			res.Reasons = append(res.Reasons, "case(s) in the baseline are absent from this run: "+strings.Join(res.Missing, ", "))
		}
	}
	res.OK = len(res.Reasons) == 0
	return res
}

// diffCases compares the two runs case by case. A case skipped on either side
// is not compared: a skip is not evidence either way.
func diffCases(baseline, current *Report) (newFailures, fixed, missing, added []string) {
	base := map[string]CaseResult{}
	for _, c := range baseline.Cases {
		base[c.ID] = c
	}
	seen := map[string]bool{}
	for _, c := range current.Cases {
		seen[c.ID] = true
		b, ok := base[c.ID]
		if !ok {
			added = append(added, c.ID)
			continue
		}
		if b.Skipped || c.Skipped {
			continue
		}
		switch {
		case b.OK && !c.OK:
			newFailures = append(newFailures, c.ID)
		case !b.OK && c.OK:
			fixed = append(fixed, c.ID)
		}
	}
	for id, b := range base {
		if !seen[id] && !b.Skipped {
			missing = append(missing, id)
		}
	}
	for _, s := range [][]string{newFailures, fixed, missing, added} {
		sort.Strings(s)
	}
	return newFailures, fixed, missing, added
}

// Render writes the gate verdict for a CI log.
func (g GateResult) Render(w io.Writer) {
	out := &lines{w: w}
	verdict := "PASS"
	if !g.OK {
		verdict = "BLOCKED"
	}
	out.printf("golden regression gate: %s\n", verdict)
	out.printf("  pass rate %.0f%% (baseline %.0f%%, drop %.1f points, allowed %.1f)\n",
		g.PassRate*100, g.BaselinePassRate*100, g.Drop*100, g.MaxDrop*100)
	for _, row := range []struct {
		label string
		ids   []string
	}{
		{"newly failing", g.NewFailures},
		{"newly fixed", g.Fixed},
		{"missing from this run", g.Missing},
		{"new since the baseline", g.Added},
	} {
		if len(row.ids) > 0 {
			out.printf("  %s: %s\n", row.label, strings.Join(row.ids, ", "))
		}
	}
	for _, r := range g.Reasons {
		out.printf("  \u2717 %s\n", r)
	}
}

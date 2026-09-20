package templates

import (
	"strings"
	"testing"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

func TestEveryKindHasValidPolicyAndAcceptance(t *testing.T) {
	kinds, err := Kinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) == 0 {
		t.Fatal("no template directories embedded")
	}
	for _, k := range kinds {
		p, err := Policy(k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("%s policy invalid: %v", k, err)
		}
		for _, tool := range p.AllowedTools {
			if tool == "Bash" {
				t.Errorf("%s: blanket Bash in a default policy is forbidden (design §7)", k)
			}
		}
		if _, err := Acceptance(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
		if _, err := Prompt(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestRenderCodeFix(t *testing.T) {
	out, err := Render(task.KindCodeFix, map[string]any{
		"Repo": "org/svc", "Ref": "main", "Number": 42, "Title": "Crash on empty input",
		"Body": "Steps: run with no args.", "URL": "https://github.com/org/svc/issues/42",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"org/svc", "#42", "Crash on empty input", "Steps: run with no args.", "issues/42", "CLAUDE.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt missing %q:\n%s", want, out)
		}
	}
}

func TestRenderMissingKeyFails(t *testing.T) {
	if _, err := Render(task.KindCodeFix, map[string]any{"Repo": "x"}); err == nil {
		t.Fatal("expected error for missing template keys")
	}
}

func TestPhasesCodeFixPlanned(t *testing.T) {
	phases, err := Phases(task.KindCodeFixPlanned)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 || phases[0].Name != "plan" || !phases[0].Review || phases[1].Name != "implement" || phases[1].Review || phases[1].Prompt == "" || phases[0].Prompt != "" {
		t.Fatalf("%+v", phases)
	}
	base, _ := Policy(task.KindCodeFixPlanned)
	acc, _ := Acceptance(task.KindCodeFixPlanned)
	specs, err := task.BuildPhases(base, acc, phases)
	if err != nil {
		t.Fatal(err)
	}
	if p := specs[0].Policy; p.Judge.Enabled || len(p.AllowedTools) != 3 || p.MaxCostUSD != 0.75 {
		t.Errorf("plan policy %+v", p)
	}
	if a := specs[0].Acceptance; !a.HasSchema() || a.ExpectChanges == nil || *a.ExpectChanges {
		t.Errorf("plan acceptance %+v", a)
	}
	if err := specs[0].Policy.Validate(); err != nil {
		t.Error(err)
	}
	if none, err := Phases(task.KindCodeFix); err != nil || none != nil {
		t.Errorf("code_fix phases %v %v", none, err)
	}
	out, err := RenderPhase(phases[1].Prompt, PhaseData{Plan: `{"summary":"x"}`, Comment: "tiny", Task: "orig"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Plan approved", `{"summary":"x"}`, "> tiny", "Never delete or weaken"} {
		if !strings.Contains(out, want) {
			t.Errorf("phase prompt missing %q:\n%s", want, out)
		}
	}
	if out, _ := RenderPhase(phases[1].Prompt, PhaseData{Plan: "{}"}); strings.Contains(out, "reviewer added") {
		t.Error("empty comment must not render the note")
	}
	if _, err := RenderPhase("{{ .Missing }}", PhaseData{}); err == nil {
		t.Error("missing key accepted")
	}
}

// TestReadOnlyKindsAreReadOnly pins the property the whole read-only pipeline
// rests on (plan Step 20): these kinds must not carry a write tool, because
// checks.DiffSanity fails any run of theirs that changes a file.
func TestReadOnlyKindsAreReadOnly(t *testing.T) {
	write := map[string]bool{"Edit": true, "Write": true, "NotebookEdit": true, "MultiEdit": true}
	for _, k := range []task.Kind{task.KindCodeReview, task.KindReport, task.KindTriage} {
		if k.ChangesWorkspace() {
			t.Errorf("%s reports that it changes the workspace", k)
		}
		p, err := Policy(k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		for _, tool := range p.AllowedTools {
			if write[tool] {
				t.Errorf("%s allows the write tool %q", k, tool)
			}
			// A bare `Bash(git *)` would let the worker commit or checkout;
			// read-only kinds get only the inspecting subcommands.
			if strings.HasPrefix(tool, "Bash(git *") {
				t.Errorf("%s allows unrestricted git: %q", k, tool)
			}
		}
		if !p.Judge.Enabled {
			t.Errorf("%s disables the judge; a read-only deliverable has no acceptance command to check it", k)
		}
	}
}

// TestReadOnlyKindsRequireStructuredOutput: the deliverable of a read-only
// kind is its structured output, so a missing schema would let a run deliver
// nothing and still pass.
func TestReadOnlyKindsRequireStructuredOutput(t *testing.T) {
	for _, k := range []task.Kind{task.KindCodeReview, task.KindReport, task.KindTriage} {
		a, err := Acceptance(k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if !a.HasSchema() {
			t.Errorf("%s sets no acceptance.json_schema", k)
		}
		if len(a.Commands) != 0 {
			t.Errorf("%s runs acceptance commands (%v); a read-only kind has no build to check", k, a.Commands)
		}
	}
}

func TestRenderNewKinds(t *testing.T) {
	data := map[string]any{
		"Repo": "org/svc", "Ref": "main", "Number": 7, "Title": "Checkout 500s",
		"Body": "It fails under load.", "URL": "https://github.com/org/svc/issues/7",
	}
	for kind, wants := range map[task.Kind][]string{
		task.KindCodeReview: {"org/svc", "#7", "Checkout 500s", "read-only", "CLAUDE.md", "JSON"},
		task.KindReport:     {"org/svc", "Checkout 500s", "read-only", "unknowns", "evidence"},
		task.KindTriage:     {"org/svc", "#7", "read-only", "missing_information", "confidence"},
	} {
		out, err := Render(kind, data)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s prompt missing %q:\n%s", kind, want, out)
			}
		}
	}
	// The issue number is optional: a cron-triggered report has none.
	out, err := Render(task.KindReport, map[string]any{"Repo": "org/svc", "Ref": "main", "Number": 0, "Title": "Weekly", "Body": "b", "URL": ""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "(#0)") || strings.Contains(out, "Source:") {
		t.Errorf("empty number/url rendered:\n%s", out)
	}
}

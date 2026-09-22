// Package templates embeds the per-kind task templates:
// templates/<kind>/{prompt.md.tmpl,policy.json,acceptance.json} plus, for
// multi-phase kinds (design §6), templates/<kind>/phases/<n>/{phase.json,prompt.md.tmpl},
// and for fan-out kinds (design §6 fan-out, plan Step 21),
// templates/<kind>/child/{task.json,prompt.md.tmpl}.
package templates

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/100xteam-ai/foreman/internal/task"
)

//go:embed */policy.json */acceptance.json */prompt.md.tmpl */phases/*/phase.json */phases/*/prompt.md.tmpl */child/task.json */child/prompt.md.tmpl
var files embed.FS

// FS exposes the embedded tree (tests and `harness eval` read it directly).
func FS() fs.FS { return files }

// Kinds lists the kinds that ship a template directory.
func Kinds() ([]task.Kind, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	var kinds []task.Kind
	for _, e := range entries {
		if e.IsDir() {
			kinds = append(kinds, task.Kind(e.Name()))
		}
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds, nil
}

// Policy loads templates/<kind>/policy.json.
func Policy(kind task.Kind) (task.Policy, error) {
	var p task.Policy
	if err := readJSON(kind, "policy.json", &p); err != nil {
		return p, err
	}
	return p, nil
}

// Acceptance loads templates/<kind>/acceptance.json.
func Acceptance(kind task.Kind) (task.Acceptance, error) {
	var a task.Acceptance
	if err := readJSON(kind, "acceptance.json", &a); err != nil {
		return a, err
	}
	return a, nil
}

// Phases loads templates/<kind>/phases/<n>/ in numeric order. Kinds without a
// phases directory return nil. Phase directories must be 1..N without gaps;
// every phase after the first needs a prompt.md.tmpl.
func Phases(kind task.Kind) ([]task.PhaseTemplate, error) {
	dir := string(kind) + "/phases"
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("phases for kind %q: %w", kind, err)
	}
	nums := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 {
			return nil, fmt.Errorf("phases for kind %q: directory %q is not a phase number", kind, e.Name())
		}
		nums = append(nums, n)
	}
	sort.Ints(nums)
	out := make([]task.PhaseTemplate, 0, len(nums))
	for i, n := range nums {
		if n != i+1 {
			return nil, fmt.Errorf("phases for kind %q: expected phase %d, found %d", kind, i+1, n)
		}
		base := fmt.Sprintf("%s/%d/", dir, n)
		var ph task.PhaseTemplate
		b, err := files.ReadFile(base + "phase.json")
		if err != nil {
			return nil, fmt.Errorf("phase %d of kind %q: %w", n, kind, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ph); err != nil {
			return nil, fmt.Errorf("parse %sphase.json: %w", base, err)
		}
		if pb, err := files.ReadFile(base + "prompt.md.tmpl"); err == nil {
			ph.Prompt = string(pb)
			if _, err := template.New("phase").Option("missingkey=error").Parse(ph.Prompt); err != nil {
				return nil, fmt.Errorf("parse %sprompt.md.tmpl: %w", base, err)
			}
		} else if n > 1 {
			return nil, fmt.Errorf("phase %d of kind %q has no prompt.md.tmpl", n, kind)
		}
		out = append(out, ph)
	}
	return out, nil
}

// Child loads templates/<kind>/child/{task.json,prompt.md.tmpl}. Kinds without
// a child directory return nil: they never fan out.
func Child(kind task.Kind) (*task.ChildTemplate, error) {
	base := string(kind) + "/child/"
	b, err := files.ReadFile(base + "task.json")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("child template for kind %q: %w", kind, err)
	}
	var ct task.ChildTemplate
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ct); err != nil {
		return nil, fmt.Errorf("parse %stask.json: %w", base, err)
	}
	pb, err := files.ReadFile(base + "prompt.md.tmpl")
	if err != nil {
		return nil, fmt.Errorf("child template for kind %q has no prompt.md.tmpl: %w", kind, err)
	}
	ct.Prompt = string(pb)
	if _, err := template.New("child").Option("missingkey=error").Parse(ct.Prompt); err != nil {
		return nil, fmt.Errorf("parse %sprompt.md.tmpl: %w", base, err)
	}
	return &ct, nil
}

// ChildData is what a fan-out child prompt template sees (plan Step 21).
type ChildData struct {
	// Title and Detail come from the planner's subtask.
	Title, Detail string
	// Files is the subtask's file list, which is also the child's diff scope.
	Files []string
	// Siblings are the other subtasks' titles, so the worker knows what it
	// must leave alone.
	Siblings []string
	// Summary is the planner's one-line summary of the whole change.
	Summary string
	// Task is the original parent prompt.
	Task string
	// Index is the child's 1-based position, Total the number of children.
	Index, Total int
}

// PhaseData is what a phase prompt template sees.
type PhaseData struct {
	// Plan is the previous phase's structured output, pretty-printed.
	Plan string
	// Comment is the reviewer's note on approval ("" when none).
	Comment string
	// Task is the original task prompt.
	Task string
	// Children is the fan-out result table the synthesizer phase merges;
	// empty for every other phase.
	Children []ChildSummary
}

// ChildSummary is one row of PhaseData.Children: what a fan-out child did,
// as the orchestrator recorded it. It never carries the child's transcript.
type ChildSummary struct {
	Title     string
	TaskID    string
	RunID     string
	Status    string
	Artifacts []string
	// Checks is the child's Layer 1 summary ("tests=pass lint=pass").
	Checks string
	// Error is the child's last failure reason when it did not deliver.
	Error string
	// Output is the child's structured output, pretty-printed ("" when none).
	Output string
}

// RenderPhase executes a phase prompt template (task.PhaseSpec.Prompt).
func RenderPhase(tmpl string, data PhaseData) (string, error) {
	t, err := template.New("phase").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("phase prompt: %w", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("render phase prompt: %w", err)
	}
	return sb.String(), nil
}

// RenderChild executes a fan-out child prompt template (task.ChildSpec.Prompt).
func RenderChild(tmpl string, data ChildData) (string, error) {
	t, err := template.New("child").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("child prompt: %w", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("render child prompt: %w", err)
	}
	return sb.String(), nil
}

// Prompt parses templates/<kind>/prompt.md.tmpl.
func Prompt(kind task.Kind) (*template.Template, error) {
	b, err := files.ReadFile(string(kind) + "/prompt.md.tmpl")
	if err != nil {
		return nil, fmt.Errorf("template for kind %q: %w", kind, err)
	}
	return template.New(string(kind)).Option("missingkey=error").Parse(string(b))
}

// Render executes the kind's prompt template with data.
func Render(kind task.Kind, data any) (string, error) {
	t, err := Prompt(kind)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, sealIssueText(data)); err != nil {
		return "", fmt.Errorf("render prompt for kind %q: %w", kind, err)
	}
	return sb.String(), nil
}

// issueFields are the template keys that carry text the issue's author wrote.
var issueFields = map[string]bool{"Title": true, "Body": true}

// sealIssueText keeps user-written text from closing the `<issue>` fence the
// prompt templates wrap it in: a body containing `</issue>` would otherwise
// end the quoted block early and put whatever follows on the same footing as
// the harness's own instructions. Only the map form (what intake builds) is
// touched; the tag is rewritten, not stripped, so the text stays readable.
func sealIssueText(data any) any {
	m, ok := data.(map[string]any)
	if !ok {
		return data
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, isStr := v.(string); isStr && issueFields[k] {
			v = issueTag.Replace(s)
		}
		out[k] = v
	}
	return out
}

var issueTag = strings.NewReplacer("</issue>", "</issue >", "</ISSUE>", "</ISSUE >", "<issue>", "<issue >", "<ISSUE>", "<ISSUE >")

func readJSON(kind task.Kind, name string, v any) error {
	b, err := files.ReadFile(string(kind) + "/" + name)
	if err != nil {
		return fmt.Errorf("template %s for kind %q: %w", name, kind, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s/%s: %w", kind, name, err)
	}
	return nil
}

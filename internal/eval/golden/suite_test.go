package golden

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
	"github.com/100xteam-ai/foreman/templates"
)

// suitePath is the shipped suite, relative to this package.
const suitePath = "../../../evals/golden"

// TestShippedSuiteLoads is the cheap half of the golden suite: it proves every
// case file parses, validates, and points at a fixture that exists, without
// spending a token. A malformed case would otherwise only surface during a
// paid nightly run.
func TestShippedSuiteLoads(t *testing.T) {
	s, err := Load(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cases) < 20 {
		t.Errorf("suite has %d cases; design §5.4 asks for 20–50", len(s.Cases))
	}
	for _, c := range s.Cases {
		// Every case must build a task the harness would actually accept:
		// the kind's template merged with the case's overrides.
		policy, err := templates.Policy(c.Kind)
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		acceptance, err := templates.Acceptance(c.Kind)
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		phases, err := templates.Phases(c.Kind)
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		child, err := templates.Child(c.Kind)
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		spec := c.Spec(filepath.Join(t.TempDir(), "origin.git"))
		if _, err := spec.Build(policy, acceptance, phases, child, time.Now()); err != nil {
			t.Errorf("%s does not build a valid task: %v", c.ID, err)
		}
	}
}

// TestShippedSuiteCoversEveryKind keeps the suite honest as kinds are added:
// a kind with a template but no golden case is a kind nothing protects.
func TestShippedSuiteCoversEveryKind(t *testing.T) {
	s, err := Load(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[task.Kind]int{}
	for _, c := range s.Cases {
		covered[c.Kind]++
	}
	kinds, err := templates.Kinds()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		if covered[k] == 0 {
			t.Errorf("kind %q has a template but no golden case", k)
		}
	}
}

// TestShippedSuiteHasNegativeCases guards the property that makes the suite
// worth running: a suite where every case expects a pass cannot catch a
// harness that delivers everything it is given.
func TestShippedSuiteHasNegativeCases(t *testing.T) {
	s, err := Load(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	byOutcome := map[Outcome]int{}
	for _, c := range s.Cases {
		byOutcome[c.Expect.Outcome]++
	}
	if byOutcome[OutcomeFail] == 0 {
		t.Error("no case expects a failure: the suite cannot catch a harness that delivers anything")
	}
	if byOutcome[OutcomeNeedsReview] == 0 {
		t.Error("no case expects human review: the routing path to needs_review is unprotected")
	}
}

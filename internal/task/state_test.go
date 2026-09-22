package task

import (
	"errors"
	"testing"
)

// legal is the §3.2 diagram, edge by edge.
var legal = map[RunStatus][]RunStatus{
	StatusQueued:      {StatusRunning},
	StatusRunning:     {StatusEvaluating},
	StatusEvaluating:  {StatusPassed, StatusFailed, StatusNeedsReview},
	StatusFailed:      {StatusQueued, StatusDead},
	StatusNeedsReview: {StatusDelivered, StatusClosed},
	StatusPassed:      {StatusDelivered},
}

func TestTransitionExhaustive(t *testing.T) {
	for _, from := range Statuses {
		for _, to := range Statuses {
			want := false
			for _, l := range legal[from] {
				if l == to {
					want = true
				}
			}
			err := Transition(from, to)
			switch {
			case want && err != nil:
				t.Errorf("%s -> %s should be legal, got %v", from, to, err)
			case !want && err == nil:
				t.Errorf("%s -> %s should be illegal", from, to)
			case !want:
				var te *TransitionError
				if !errors.As(err, &te) || te.From != from || te.To != to {
					t.Errorf("%s -> %s: want TransitionError, got %v", from, to, err)
				}
			}
		}
	}
}

func TestTransitionUnknownStatus(t *testing.T) {
	if err := Transition("bogus", StatusRunning); err == nil {
		t.Error("unknown from-status accepted")
	}
	if err := Transition(StatusQueued, "bogus"); err == nil {
		t.Error("unknown to-status accepted")
	}
	var te *TransitionError
	if errors.As(Transition("bogus", StatusRunning), &te) {
		t.Error("unknown status must not be reported as a TransitionError")
	}
}

func TestTerminal(t *testing.T) {
	for _, s := range Statuses {
		want := s == StatusDelivered || s == StatusDead || s == StatusClosed
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
	if RunStatus("bogus").Terminal() {
		t.Error("unknown status reported terminal")
	}
}

func TestEveryStatusReachable(t *testing.T) {
	seen := map[RunStatus]bool{StatusQueued: true}
	frontier := []RunStatus{StatusQueued}
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		for _, n := range Next(s) {
			if !seen[n] {
				seen[n] = true
				frontier = append(frontier, n)
			}
		}
	}
	for _, s := range Statuses {
		if !seen[s] {
			t.Errorf("%s unreachable from queued", s)
		}
	}
}

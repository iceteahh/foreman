package task

import "fmt"

// RunStatus is a node in the design §3.2 state machine.
type RunStatus string

const (
	StatusQueued      RunStatus = "queued"
	StatusRunning     RunStatus = "running"
	StatusEvaluating  RunStatus = "evaluating"
	StatusPassed      RunStatus = "passed"
	StatusFailed      RunStatus = "failed"
	StatusNeedsReview RunStatus = "needs_review"
	StatusDead        RunStatus = "dead"
	StatusDelivered   RunStatus = "delivered"
	StatusClosed      RunStatus = "closed"
)

// Statuses lists every status in declaration order.
var Statuses = []RunStatus{
	StatusQueued, StatusRunning, StatusEvaluating, StatusPassed, StatusFailed,
	StatusNeedsReview, StatusDead, StatusDelivered, StatusClosed,
}

// transitions encodes the §3.2 diagram exactly. Anything not listed is illegal.
var transitions = map[RunStatus][]RunStatus{
	StatusQueued:     {StatusRunning},
	StatusRunning:    {StatusEvaluating},
	StatusEvaluating: {StatusPassed, StatusFailed, StatusNeedsReview},
	StatusFailed:     {StatusQueued, StatusDead},
	// A reviewed run never goes back to `queued`. Rejecting one closes it and
	// queues a *new* run (orchestrator.Decide → retry), so the record of what
	// the human saw stays intact and the retry gets its own attempt number. An
	// unreachable edge here would be a licence to rewind a reviewed run.
	StatusNeedsReview: {StatusDelivered, StatusClosed},
	StatusPassed:      {StatusDelivered},
	StatusDelivered:   {},
	StatusDead:        {},
	StatusClosed:      {},
}

// Valid reports whether s is a known status.
func (s RunStatus) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether no transition leaves s.
func (s RunStatus) Terminal() bool {
	next, ok := transitions[s]
	return ok && len(next) == 0
}

// TransitionError is returned for an illegal edge.
type TransitionError struct {
	From, To RunStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal run transition %s -> %s", e.From, e.To)
}

// Transition returns nil when from -> to is a legal edge.
func Transition(from, to RunStatus) error {
	next, ok := transitions[from]
	if !ok {
		return fmt.Errorf("unknown run status %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("unknown run status %q", to)
	}
	for _, n := range next {
		if n == to {
			return nil
		}
	}
	return &TransitionError{From: from, To: to}
}

// Next returns the legal successors of s (copy).
func Next(s RunStatus) []RunStatus {
	out := make([]RunStatus, len(transitions[s]))
	copy(out, transitions[s])
	return out
}

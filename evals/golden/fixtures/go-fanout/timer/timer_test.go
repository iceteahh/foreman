package timer

import (
	"testing"
	"time"
)

func TestObserveAndMean(t *testing.T) {
	tm := &Timer{Name: "request_duration"}
	tm.Observe(10 * time.Millisecond)
	tm.Observe(20 * time.Millisecond)
	if got := tm.Mean(); got != 15*time.Millisecond {
		t.Fatalf("mean %v, want 15ms", got)
	}
}

// Package timer holds a metric that records durations.
package timer

import "time"

// Timer accumulates observed durations.
type Timer struct {
	Name   string
	Labels map[string]string
	Total  time.Duration
	Count  int64
}

// Observe records one duration.
func (t *Timer) Observe(d time.Duration) {
	t.Total += d
	t.Count++
}

// Mean returns the average observed duration, or 0 when nothing was observed.
func (t *Timer) Mean() time.Duration {
	if t.Count == 0 {
		return 0
	}
	return t.Total / time.Duration(t.Count)
}

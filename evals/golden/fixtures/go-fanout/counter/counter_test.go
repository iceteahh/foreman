package counter

import "testing"

func TestIncAndAdd(t *testing.T) {
	c := &Counter{Name: "requests"}
	c.Inc()
	c.Add(41)
	c.Add(-5)
	if c.Value != 42 {
		t.Fatalf("value %d, want 42", c.Value)
	}
}

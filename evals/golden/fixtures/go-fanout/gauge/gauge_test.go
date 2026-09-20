package gauge

import "testing"

func TestSetAndAdd(t *testing.T) {
	g := &Gauge{Name: "queue_depth"}
	g.Set(10)
	g.Add(-4)
	if g.Value != 6 {
		t.Fatalf("value %v, want 6", g.Value)
	}
}

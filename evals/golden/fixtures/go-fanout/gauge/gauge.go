// Package gauge holds a metric that can move in both directions.
package gauge

// Gauge is a value that goes up and down.
type Gauge struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Set replaces the gauge's value.
func (g *Gauge) Set(v float64) { g.Value = v }

// Add moves the gauge by delta.
func (g *Gauge) Add(delta float64) { g.Value += delta }

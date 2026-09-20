// Package counter holds a monotonically increasing metric.
package counter

// Counter counts events that only ever go up.
type Counter struct {
	Name   string
	Labels map[string]string
	Value  int64
}

// Inc adds one to the counter.
func (c *Counter) Inc() { c.Value++ }

// Add adds n to the counter. Negative n is ignored: a counter never goes down.
func (c *Counter) Add(n int64) {
	if n > 0 {
		c.Value += n
	}
}

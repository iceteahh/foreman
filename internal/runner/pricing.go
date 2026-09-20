package runner

import (
	"strings"
	"sync"

	"github.com/100xteam-ai/foreman/internal/events"
)

// Price is USD per million tokens for one model.
type Price struct {
	Input, Output float64
	CacheWrite5m  float64
	CacheWrite1h  float64
	CacheRead     float64
}

// PriceTable maps a model id (or canonical prefix) to its price. Pinned; the
// authoritative cost is always result.total_cost_usd. This table only feeds the
// mid-run kill backstop, so erring high is fine.
type PriceTable struct {
	Prices  map[string]Price
	Default Price
}

// DefaultPrices is verified against the captured fixtures for haiku-4-5
// (result_success.json and result_budget_exhausted.json reproduce exactly).
// Other rows are the published list prices at the time of pinning.
var DefaultPrices = PriceTable{
	Prices: map[string]Price{
		"claude-haiku-4-5":  {Input: 1, Output: 5, CacheWrite5m: 1.25, CacheWrite1h: 2, CacheRead: 0.10},
		"claude-sonnet-4-5": {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30},
		"claude-sonnet-5":   {Input: 3, Output: 15, CacheWrite5m: 3.75, CacheWrite1h: 6, CacheRead: 0.30},
		"claude-opus-4-1":   {Input: 15, Output: 75, CacheWrite5m: 18.75, CacheWrite1h: 30, CacheRead: 1.50},
		"claude-opus-4-5":   {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50},
		"claude-opus-5":     {Input: 5, Output: 25, CacheWrite5m: 6.25, CacheWrite1h: 10, CacheRead: 0.50},
		"claude-fable-5-1":  {Input: 15, Output: 75, CacheWrite5m: 18.75, CacheWrite1h: 30, CacheRead: 1.50},
	},
	// Unknown models are priced like the most expensive known row so the
	// backstop trips early rather than late.
	Default: Price{Input: 15, Output: 75, CacheWrite5m: 18.75, CacheWrite1h: 30, CacheRead: 1.50},
}

// Lookup returns the price for model, matching by longest known prefix.
func (t PriceTable) Lookup(model string) Price {
	best, bestLen := t.Default, -1
	for k, p := range t.Prices {
		if strings.HasPrefix(model, k) && len(k) > bestLen {
			best, bestLen = p, len(k)
		}
	}
	return best
}

// Estimate prices one message's usage in USD.
func (t PriceTable) Estimate(model string, u events.Usage) float64 {
	p := t.Lookup(model)
	w5, w1 := u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
	if w5+w1 == 0 {
		// Older shape without the split: assume the dearer TTL.
		w1 = u.CacheCreationInputTokens
	}
	per := 1e-6
	return per * (float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(w5)*p.CacheWrite5m +
		float64(w1)*p.CacheWrite1h +
		float64(u.CacheReadInputTokens)*p.CacheRead)
}

// CostTracker accumulates an estimate across assistant events. The CLI emits
// several assistant events for one API message (one per content block) that all
// carry that message's usage, so usage is keyed by message id and the latest
// value wins (usage grows as the message streams).
type CostTracker struct {
	table PriceTable
	mu    sync.Mutex
	msgs  map[string]float64
	total float64
}

// NewCostTracker returns an empty tracker.
func NewCostTracker(table PriceTable) *CostTracker {
	return &CostTracker{table: table, msgs: map[string]float64{}}
}

// Observe records an assistant event and returns the running total.
func (c *CostTracker) Observe(a *events.AssistantEvent) float64 {
	if a == nil {
		return c.Total()
	}
	cost := c.table.Estimate(a.Message.Model, a.Message.Usage)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := a.Message.ID
	if key == "" {
		c.total += cost
		return c.total
	}
	c.total += cost - c.msgs[key]
	c.msgs[key] = cost
	return c.total
}

// Total returns the running estimate.
func (c *CostTracker) Total() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

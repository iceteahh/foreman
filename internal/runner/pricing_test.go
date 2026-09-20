package runner

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
)

func loadResult(t *testing.T, name string) *events.ResultEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	ev := events.Parse(b, 1)
	if ev.Result == nil {
		t.Fatalf("%s is not a result", name)
	}
	return ev.Result
}

// The price table must reproduce the CLI's own cost for the captured fixtures.
func TestEstimateMatchesFixtures(t *testing.T) {
	for _, name := range []string{"result_success.json", "result_budget_exhausted.json", "result_resumed.json", "result_forked.json"} {
		r := loadResult(t, name)
		for model, mu := range r.ModelUsage {
			u := events.Usage{InputTokens: mu.InputTokens, OutputTokens: mu.OutputTokens,
				CacheReadInputTokens: mu.CacheReadInputTokens, CacheCreationInputTokens: mu.CacheCreationInputTokens}
			// Fixtures were captured with 1h cache writes (usage.cache_creation shows it).
			u.CacheCreation.Ephemeral1h = mu.CacheCreationInputTokens
			got := DefaultPrices.Estimate(model, u)
			if math.Abs(got-mu.CostUSD) > 1e-9 {
				t.Errorf("%s %s: estimate %.7f, CLI %.7f", name, model, got, mu.CostUSD)
			}
		}
	}
}

func TestLookupPrefixAndDefault(t *testing.T) {
	if p := DefaultPrices.Lookup("claude-haiku-4-5-20251001"); p.Input != 1 {
		t.Errorf("haiku lookup %+v", p)
	}
	if p := DefaultPrices.Lookup("claude-unknown-9"); p != DefaultPrices.Default {
		t.Errorf("unknown model should use default: %+v", p)
	}
}

func TestCostTrackerDedupesMessageID(t *testing.T) {
	c := NewCostTracker(DefaultPrices)
	mk := func(id string, out int) *events.AssistantEvent {
		var a events.AssistantEvent
		_ = json.Unmarshal([]byte(`{"message":{"id":"`+id+`","model":"claude-haiku-4-5","usage":{"input_tokens":1000,"output_tokens":0}}}`), &a)
		a.Message.Usage.OutputTokens = out
		return &a
	}
	c.Observe(mk("m1", 10))
	c.Observe(mk("m1", 100)) // same message, streamed further: replaces, not adds
	total := c.Observe(mk("m2", 100))
	want := 2 * (1000*1e-6*1 + 100*1e-6*5)
	if math.Abs(total-want) > 1e-12 {
		t.Errorf("total %.9f want %.9f", total, want)
	}
	if c.Observe(nil) != total {
		t.Error("nil event changed total")
	}
}

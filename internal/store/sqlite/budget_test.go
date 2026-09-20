package sqlite

import (
	"context"
	"testing"

	"github.com/100xteam-ai/foreman/internal/budget"
)

func TestBudgetSpendUpsert(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	day := "2026-09-15"
	if usd, runs, err := s.GetSpend(ctx, "code_fix", day); err != nil || usd != 0 || runs != 0 {
		t.Fatalf("absent row should be zero: %v %v %v", usd, runs, err)
	}
	total, err := s.AddSpend(ctx, "code_fix", day, 0.25, 1)
	if err != nil || total != 0.25 {
		t.Fatalf("first add: %v %v", total, err)
	}
	if total, err = s.AddSpend(ctx, "code_fix", day, 0.5, 1); err != nil || total != 0.75 {
		t.Fatalf("second add: %v %v", total, err)
	}
	if _, err := s.AddSpend(ctx, budget.Global, day, 0.75, 2); err != nil {
		t.Fatal(err)
	}
	// A different day is a different row.
	if _, err := s.AddSpend(ctx, "code_fix", "2026-09-16", 9, 1); err != nil {
		t.Fatal(err)
	}
	usd, runs, err := s.GetSpend(ctx, "code_fix", day)
	if err != nil || usd != 0.75 || runs != 2 {
		t.Errorf("get %v %v %v", usd, runs, err)
	}
	rows, err := s.ListSpend(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Key != "code_fix" || rows[1].Key != budget.Global || rows[1].USD != 0.75 {
		t.Errorf("list %+v", rows)
	}
	for _, r := range rows {
		if r.Day != day {
			t.Errorf("day %q", r.Day)
		}
	}
	if _, err := s.AddSpend(ctx, "", day, 1, 1); err == nil {
		t.Error("empty key accepted")
	}
}

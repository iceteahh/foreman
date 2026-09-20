package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/100xteam-ai/foreman/internal/budget"
)

// AddSpend implements budget.Store: upsert (key, day) and return the new total.
func (s *Store) AddSpend(ctx context.Context, key, day string, usd float64, runs int) (float64, error) {
	if key == "" || day == "" {
		return 0, errors.New("budget: key and day are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_spend(key, day, usd, runs, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(key, day) DO UPDATE SET usd = usd + excluded.usd, runs = runs + excluded.runs, updated_at = excluded.updated_at`,
		key, day, usd, runs, ts(s.now())); err != nil {
		return 0, fmt.Errorf("add spend %s/%s: %w", key, day, err)
	}
	var total float64
	if err := tx.QueryRowContext(ctx, `SELECT usd FROM budget_spend WHERE key = ? AND day = ?`, key, day).Scan(&total); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// GetSpend implements budget.Store; an absent row is zero spend.
func (s *Store) GetSpend(ctx context.Context, key, day string) (float64, int, error) {
	var usd float64
	var runs int
	err := s.db.QueryRowContext(ctx, `SELECT usd, runs FROM budget_spend WHERE key = ? AND day = ?`, key, day).Scan(&usd, &runs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return usd, runs, nil
}

// ListSpend implements budget.Store.
func (s *Store) ListSpend(ctx context.Context, day string) ([]budget.Spend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, usd, runs FROM budget_spend WHERE day = ? ORDER BY key`, day)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []budget.Spend
	for rows.Next() {
		sp := budget.Spend{Day: day}
		if err := rows.Scan(&sp.Key, &sp.USD, &sp.Runs); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

var _ budget.Store = (*Store)(nil)

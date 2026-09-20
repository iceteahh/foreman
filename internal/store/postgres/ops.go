package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/budget"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/deadletter"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
)

// AddSpend implements budget.Store: upsert (key, day) and return the new
// total. The upsert is atomic in one statement, so replicas recording spend
// concurrently cannot lose each other's dollars.
func (s *Store) AddSpend(ctx context.Context, key, day string, usd float64, runs int) (float64, error) {
	if key == "" || day == "" {
		return 0, errors.New("budget: key and day are required")
	}
	var total float64
	err := s.pool.QueryRow(ctx, `INSERT INTO budget_spend(key, day, usd, runs, updated_at) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (key, day) DO UPDATE SET usd = budget_spend.usd + excluded.usd, runs = budget_spend.runs + excluded.runs,
			updated_at = excluded.updated_at
		RETURNING usd`, key, day, usd, runs, s.now().UTC()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("add spend %s/%s: %w", key, day, err)
	}
	return total, nil
}

// GetSpend implements budget.Store; an absent row is zero spend.
func (s *Store) GetSpend(ctx context.Context, key, day string) (float64, int, error) {
	var usd float64
	var runs int
	err := s.pool.QueryRow(ctx, `SELECT usd, runs FROM budget_spend WHERE key = $1 AND day = $2`, key, day).Scan(&usd, &runs)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return usd, runs, nil
}

// ListSpend implements budget.Store.
func (s *Store) ListSpend(ctx context.Context, day string) ([]budget.Spend, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, usd, runs FROM budget_spend WHERE day = $1 ORDER BY key`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// SaveEntry implements deadletter.Store (upsert keyed by run).
func (s *Store) SaveEntry(ctx context.Context, e deadletter.Entry) error {
	if e.RunID == "" || e.TaskID == "" {
		return errors.New("dead letter needs run_id and task_id")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO dead_letters(run_id, task_id, kind, reason, attempt, phase, feedback, event_log, dead_at, paged_at, requeued_at, requeue_run)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (run_id) DO UPDATE SET reason = excluded.reason, attempt = excluded.attempt, phase = excluded.phase,
			feedback = excluded.feedback, event_log = excluded.event_log, dead_at = excluded.dead_at`,
		e.RunID, e.TaskID, e.Kind, e.Reason, e.Attempt, e.Phase, e.Feedback, e.EventLogURI, e.DeadAt.UTC(),
		utcPtr(e.PagedAt), utcPtr(e.RequeuedAt), nullable(e.RequeueRunID))
	if err != nil {
		return fmt.Errorf("save dead letter %s: %w", e.RunID, err)
	}
	return nil
}

const deadLetterSelect = `SELECT run_id, task_id, kind, reason, attempt, phase, feedback, event_log, dead_at, paged_at, requeued_at, requeue_run FROM dead_letters`

// GetEntry implements deadletter.Store.
func (s *Store) GetEntry(ctx context.Context, runID string) (*deadletter.Entry, error) {
	entries, err := s.listEntries(ctx, deadLetterSelect+` WHERE run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, deadletter.ErrNotFound
	}
	return &entries[0], nil
}

// ListOpen implements deadletter.Store.
func (s *Store) ListOpen(ctx context.Context, limit int) ([]deadletter.Entry, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.listEntries(ctx, deadLetterSelect+` WHERE requeued_at IS NULL ORDER BY dead_at LIMIT $1`, limit)
}

// MarkPaged implements deadletter.Store.
func (s *Store) MarkPaged(ctx context.Context, runID string, at time.Time) error {
	return s.touchDeadLetter(ctx, `UPDATE dead_letters SET paged_at = $1 WHERE run_id = $2`, at.UTC(), runID)
}

// MarkRequeued implements deadletter.Store.
func (s *Store) MarkRequeued(ctx context.Context, runID, newRunID string, at time.Time) error {
	return s.touchDeadLetter(ctx, `UPDATE dead_letters SET requeued_at = $1, requeue_run = $2 WHERE run_id = $3`, at.UTC(), newRunID, runID)
}

func (s *Store) touchDeadLetter(ctx context.Context, q string, args ...any) error {
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return deadletter.ErrNotFound
	}
	return nil
}

func (s *Store) listEntries(ctx context.Context, q string, args ...any) ([]deadletter.Entry, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deadletter.Entry
	for rows.Next() {
		var e deadletter.Entry
		var requeueRun *string
		if err := rows.Scan(&e.RunID, &e.TaskID, &e.Kind, &e.Reason, &e.Attempt, &e.Phase, &e.Feedback, &e.EventLogURI,
			&e.DeadAt, &e.PagedAt, &e.RequeuedAt, &requeueRun); err != nil {
			return nil, err
		}
		if requeueRun != nil {
			e.RequeueRunID = *requeueRun
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AcquireLease implements store.Leases. One statement does the whole thing:
// insert the lease, or take it over when it is ours already or has expired.
// `WHERE` on the conflict target is what makes it a compare-and-swap — a
// replica that loses simply updates no row and learns it lost (plan Step 22).
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if name == "" || owner == "" {
		return false, errors.New("lease: name and owner are required")
	}
	if ttl <= 0 {
		return false, errors.New("lease: ttl must be > 0")
	}
	now := s.now().UTC()
	tag, err := s.pool.Exec(ctx, `INSERT INTO leases(name, owner, expires_at, updated_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (name) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at, updated_at = excluded.updated_at
		WHERE leases.owner = excluded.owner OR leases.expires_at <= excluded.updated_at`,
		name, owner, now.Add(ttl), now)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s: %w", name, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseLease implements store.Leases.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM leases WHERE name = $1 AND owner = $2`, name, owner)
	return err
}

var (
	_ budget.Store     = (*Store)(nil)
	_ deadletter.Store = (*Store)(nil)
	_ store.Leases     = (*Store)(nil)
)

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/100xteam-ai/foreman/internal/deadletter"
)

// SaveEntry implements deadletter.Store (upsert keyed by run).
func (s *Store) SaveEntry(ctx context.Context, e deadletter.Entry) error {
	if e.RunID == "" || e.TaskID == "" {
		return errors.New("dead letter needs run_id and task_id")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO dead_letters(run_id, task_id, kind, reason, attempt, phase, feedback, event_log, dead_at, paged_at, requeued_at, requeue_run)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET reason = excluded.reason, attempt = excluded.attempt, phase = excluded.phase,
			feedback = excluded.feedback, event_log = excluded.event_log, dead_at = excluded.dead_at`,
		e.RunID, e.TaskID, e.Kind, e.Reason, e.Attempt, e.Phase, e.Feedback, e.EventLogURI, ts(e.DeadAt),
		tsPtr(e.PagedAt), tsPtr(e.RequeuedAt), nullString(e.RequeueRunID))
	if err != nil {
		return fmt.Errorf("save dead letter %s: %w", e.RunID, err)
	}
	return nil
}

// GetEntry implements deadletter.Store.
func (s *Store) GetEntry(ctx context.Context, runID string) (*deadletter.Entry, error) {
	entries, err := s.listEntries(ctx, deadLetterSelect+` WHERE run_id = ?`, runID)
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
	return s.listEntries(ctx, deadLetterSelect+` WHERE requeued_at IS NULL ORDER BY dead_at LIMIT ?`, limit)
}

// MarkPaged implements deadletter.Store.
func (s *Store) MarkPaged(ctx context.Context, runID string, at time.Time) error {
	return s.touchDeadLetter(ctx, `UPDATE dead_letters SET paged_at = ? WHERE run_id = ?`, ts(at), runID)
}

// MarkRequeued implements deadletter.Store.
func (s *Store) MarkRequeued(ctx context.Context, runID, newRunID string, at time.Time) error {
	return s.touchDeadLetter(ctx, `UPDATE dead_letters SET requeued_at = ?, requeue_run = ? WHERE run_id = ?`, ts(at), newRunID, runID)
}

func (s *Store) touchDeadLetter(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return deadletter.ErrNotFound
	}
	return nil
}

const deadLetterSelect = `SELECT run_id, task_id, kind, reason, attempt, phase, feedback, event_log, dead_at, paged_at, requeued_at, requeue_run FROM dead_letters`

func (s *Store) listEntries(ctx context.Context, q string, args ...any) ([]deadletter.Entry, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []deadletter.Entry
	for rows.Next() {
		var e deadletter.Entry
		var dead string
		var paged, requeued, requeueRun sql.NullString
		if err := rows.Scan(&e.RunID, &e.TaskID, &e.Kind, &e.Reason, &e.Attempt, &e.Phase, &e.Feedback, &e.EventLogURI,
			&dead, &paged, &requeued, &requeueRun); err != nil {
			return nil, err
		}
		e.DeadAt = parseTime(dead)
		e.PagedAt = parseTimePtr(paged)
		e.RequeuedAt = parseTimePtr(requeued)
		if requeueRun.Valid {
			e.RequeueRunID = requeueRun.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ deadletter.Store = (*Store)(nil)

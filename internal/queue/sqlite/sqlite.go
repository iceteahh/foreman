// Package sqlite implements queue.Queue on a SQLite table with lease columns.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/queue"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       TEXT NOT NULL,
    task_id      TEXT NOT NULL,
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,            -- ready | leased | done | dead
    attempts     INTEGER NOT NULL DEFAULT 0,
    available_at TEXT NOT NULL,
    leased_until TEXT,
    lease_token  TEXT,
    reason       TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_lease_idx ON jobs(state, kind, available_at, priority);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_run_active_idx ON jobs(run_id) WHERE state IN ('ready','leased');
`

// Queue shares the store's *sql.DB (single writer) or owns its own.
type Queue struct {
	db  *sql.DB
	now func() time.Time
}

// New prepares the jobs table on db.
func New(db *sql.DB) (*Queue, error) {
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		return nil, fmt.Errorf("queue schema: %w", err)
	}
	return &Queue{db: db, now: time.Now}, nil
}

// SetClock overrides time for tests.
func (q *Queue) SetClock(now func() time.Time) { q.now = now }

// sqlTime is RFC3339 with a **fixed-width** nanosecond fraction. time.RFC3339Nano
// trims trailing zeros, which makes the fraction variable width — and these
// timestamps are compared as TEXT by SQLite, so a trimmed one sorts wrong:
// "…53.000327Z" > "…53.000327852Z", because 'Z' > '8'. A job enqueued a few
// microseconds earlier than the lease then fails `available_at <= now` and
// goes invisible until the clock moves on. Parsing still accepts the old
// variable-width rows (time.Parse ignores fraction width).
const sqlTime = "2006-01-02T15:04:05.000000000Z07:00"

func ts(t time.Time) string { return t.UTC().Format(sqlTime) }

func parseTS(s sql.NullString) time.Time {
	if !s.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s.String)
	return t
}

// Enqueue implements queue.Queue.
func (q *Queue) Enqueue(ctx context.Context, j queue.Job, delay time.Duration) (int64, error) {
	if j.RunID == "" || j.TaskID == "" || j.Kind == "" {
		return 0, errors.New("queue: job needs run_id, task_id and kind")
	}
	now := q.now()
	res, err := q.db.ExecContext(ctx, `INSERT INTO jobs(run_id, task_id, kind, priority, state, attempts, available_at, created_at, updated_at)
		VALUES (?,?,?,?, 'ready', 0, ?, ?, ?)`, j.RunID, j.TaskID, j.Kind, j.Priority, ts(now.Add(delay)), ts(now), ts(now))
	if err != nil {
		return 0, fmt.Errorf("queue enqueue: %w", err)
	}
	return res.LastInsertId()
}

// Lease implements queue.Queue.
func (q *Queue) Lease(ctx context.Context, kinds []string, leaseFor time.Duration) (*queue.Job, error) {
	now := q.now()
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	where := `(state = 'ready' AND available_at <= ?) OR (state = 'leased' AND leased_until < ?)`
	args := []any{ts(now), ts(now)}
	if len(kinds) > 0 {
		where = "(" + where + ") AND kind IN (?" + strings.Repeat(",?", len(kinds)-1) + ")" //nolint:gosec // placeholders only; values are bound below
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	var j queue.Job
	var avail, created sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, run_id, task_id, kind, priority, attempts, available_at, created_at FROM jobs
		WHERE `+where+` ORDER BY priority DESC, available_at ASC, id ASC LIMIT 1`, args...).
		Scan(&j.ID, &j.RunID, &j.TaskID, &j.Kind, &j.Priority, &j.Attempts, &avail, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, queue.ErrEmpty
	}
	if err != nil {
		return nil, err
	}
	token := newToken()
	until := now.Add(leaseFor)
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'leased', attempts = attempts + 1, leased_until = ?, lease_token = ?, updated_at = ?
		WHERE id = ? AND ((state = 'ready') OR (state = 'leased' AND leased_until < ?))`, ts(until), token, ts(now), j.ID, ts(now))
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, queue.ErrEmpty // raced; caller retries
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Attempts++
	j.LeaseToken = token
	j.LeasedUntil = until
	j.AvailableAt = parseTS(avail)
	j.CreatedAt = parseTS(created)
	return &j, nil
}

func (q *Queue) withLease(ctx context.Context, j *queue.Job, set string, args ...any) error {
	if j == nil || j.LeaseToken == "" {
		return queue.ErrLeaseLost
	}
	args = append(args, j.ID, j.LeaseToken)
	res, err := q.db.ExecContext(ctx, `UPDATE jobs SET `+set+`, updated_at = ? WHERE id = ? AND state = 'leased' AND lease_token = ?`, //nolint:gosec // set is a constant fragment from this file; values are bound
		append([]any{}, append(args[:len(args)-2], ts(q.now()), args[len(args)-2], args[len(args)-1])...)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

// Extend implements queue.Queue.
func (q *Queue) Extend(ctx context.Context, j *queue.Job, leaseFor time.Duration) error {
	until := q.now().Add(leaseFor)
	if err := q.withLease(ctx, j, `leased_until = ?`, ts(until)); err != nil {
		return err
	}
	j.LeasedUntil = until
	return nil
}

// Ack implements queue.Queue.
func (q *Queue) Ack(ctx context.Context, j *queue.Job) error {
	return q.withLease(ctx, j, `state = 'done', lease_token = NULL, leased_until = NULL`)
}

// Nack implements queue.Queue.
func (q *Queue) Nack(ctx context.Context, j *queue.Job, delay time.Duration) error {
	return q.withLease(ctx, j, `state = 'ready', lease_token = NULL, leased_until = NULL, available_at = ?`, ts(q.now().Add(delay)))
}

// DeadLetter implements queue.Queue.
func (q *Queue) DeadLetter(ctx context.Context, j *queue.Job, reason string) error {
	return q.withLease(ctx, j, `state = 'dead', lease_token = NULL, leased_until = NULL, reason = ?`, reason)
}

// Depth implements queue.Queue.
func (q *Queue) Depth(ctx context.Context) (queue.Depth, error) {
	var d queue.Depth
	now := q.now()
	rows, err := q.db.QueryContext(ctx, `SELECT state, COUNT(*), MIN(available_at) FROM jobs WHERE state IN ('ready','leased','dead') GROUP BY state`)
	if err != nil {
		return d, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var n int
		var oldest sql.NullString
		if err := rows.Scan(&state, &n, &oldest); err != nil {
			return d, err
		}
		switch state {
		case "ready":
			d.Ready = n
			if t := parseTS(oldest); !t.IsZero() && t.Before(now) {
				d.OldestReadyAge = now.Sub(t)
			}
		case "leased":
			d.Leased = n
		case "dead":
			d.Dead = n
		}
	}
	return d, rows.Err()
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ queue.Queue = (*Queue)(nil)

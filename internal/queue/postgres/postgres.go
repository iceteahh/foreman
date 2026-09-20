// Package postgres implements queue.Queue on Postgres with `FOR UPDATE SKIP
// LOCKED`, the mechanism a job library like River is built on (plan Step 22).
// Keeping the queue in our own table rather than behind a library keeps one
// migration path, one lease model and one set of semantics shared with the
// SQLite queue — the interface above it does not change.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/queue"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id           BIGSERIAL PRIMARY KEY,
    run_id       TEXT NOT NULL,
    task_id      TEXT NOT NULL,
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,            -- ready | leased | done | dead
    attempts     INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL,
    leased_until TIMESTAMPTZ,
    lease_token  TEXT,
    reason       TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_lease_idx ON jobs(state, kind, priority DESC, available_at) WHERE state IN ('ready','leased');
CREATE UNIQUE INDEX IF NOT EXISTS jobs_run_active_idx ON jobs(run_id) WHERE state IN ('ready','leased');
`

// Queue is the Postgres implementation. It shares the store's pool.
type Queue struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// New prepares the jobs table on pool.
func New(ctx context.Context, pool *pgxpool.Pool) (*Queue, error) {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return nil, fmt.Errorf("queue schema: %w", err)
	}
	return &Queue{pool: pool, now: time.Now}, nil
}

// SetClock overrides time for tests.
func (q *Queue) SetClock(now func() time.Time) { q.now = now }

// Enqueue implements queue.Queue.
func (q *Queue) Enqueue(ctx context.Context, j queue.Job, delay time.Duration) (int64, error) {
	if j.RunID == "" || j.TaskID == "" || j.Kind == "" {
		return 0, errors.New("queue: job needs run_id, task_id and kind")
	}
	now := q.now().UTC()
	var id int64
	err := q.pool.QueryRow(ctx, `INSERT INTO jobs(run_id, task_id, kind, priority, state, attempts, available_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'ready',0,$5,$6,$6) RETURNING id`,
		j.RunID, j.TaskID, j.Kind, j.Priority, now.Add(delay), now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("queue enqueue: %w", err)
	}
	return id, nil
}

// Lease implements queue.Queue. SKIP LOCKED is what lets every orchestrator
// replica poll the same table without blocking on each other: a row another
// replica is claiming is stepped over, not waited for.
func (q *Queue) Lease(ctx context.Context, kinds []string, leaseFor time.Duration) (*queue.Job, error) {
	now := q.now().UTC()
	token := newToken()
	// One statement: pick the best candidate, lock it, and claim it. There is
	// no window between the select and the update for another replica to take
	// the same job.
	sql := `WITH candidate AS (
			SELECT id FROM jobs
			WHERE ((state = 'ready' AND available_at <= $1) OR (state = 'leased' AND leased_until < $1))
			  AND ($4::text[] IS NULL OR kind = ANY($4))
			ORDER BY priority DESC, available_at ASC, id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs SET state = 'leased', attempts = attempts + 1, leased_until = $2, lease_token = $3, updated_at = $1
		WHERE id IN (SELECT id FROM candidate)
		RETURNING id, run_id, task_id, kind, priority, attempts, available_at, created_at`
	var kindArg any
	if len(kinds) > 0 {
		kindArg = kinds
	}
	var j queue.Job
	err := q.pool.QueryRow(ctx, sql, now, now.Add(leaseFor), token, kindArg).
		Scan(&j.ID, &j.RunID, &j.TaskID, &j.Kind, &j.Priority, &j.Attempts, &j.AvailableAt, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, queue.ErrEmpty
	}
	if err != nil {
		return nil, err
	}
	j.LeaseToken = token
	j.LeasedUntil = now.Add(leaseFor)
	return &j, nil
}

func (q *Queue) withLease(ctx context.Context, j *queue.Job, set string, args ...any) error {
	if j == nil || j.LeaseToken == "" {
		return queue.ErrLeaseLost
	}
	args = append(args, q.now().UTC(), j.ID, j.LeaseToken)
	sql := fmt.Sprintf(`UPDATE jobs SET %s, updated_at = $%d WHERE id = $%d AND state = 'leased' AND lease_token = $%d`,
		set, len(args)-2, len(args)-1, len(args)) //nolint:gosec // set is a constant fragment from this file; every value is bound
	tag, err := q.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

// Extend implements queue.Queue.
func (q *Queue) Extend(ctx context.Context, j *queue.Job, leaseFor time.Duration) error {
	until := q.now().UTC().Add(leaseFor)
	if err := q.withLease(ctx, j, `leased_until = $1`, until); err != nil {
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
	return q.withLease(ctx, j, `state = 'ready', lease_token = NULL, leased_until = NULL, available_at = $1`,
		q.now().UTC().Add(delay))
}

// DeadLetter implements queue.Queue.
func (q *Queue) DeadLetter(ctx context.Context, j *queue.Job, reason string) error {
	return q.withLease(ctx, j, `state = 'dead', lease_token = NULL, leased_until = NULL, reason = $1`, reason)
}

// Depth implements queue.Queue.
func (q *Queue) Depth(ctx context.Context) (queue.Depth, error) {
	var d queue.Depth
	now := q.now().UTC()
	rows, err := q.pool.Query(ctx, `SELECT state, COUNT(*), MIN(available_at) FROM jobs WHERE state IN ('ready','leased','dead') GROUP BY state`)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		var oldest *time.Time
		if err := rows.Scan(&state, &n, &oldest); err != nil {
			return d, err
		}
		switch state {
		case "ready":
			d.Ready = n
			if oldest != nil && oldest.Before(now) {
				d.OldestReadyAge = now.Sub(*oldest)
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

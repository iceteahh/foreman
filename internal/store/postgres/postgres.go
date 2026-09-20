// Package postgres implements store.Store on pgx for the scaled topology
// (plan Step 22, design §10): managed Postgres behind stateless orchestrator
// replicas. It is a drop-in for the SQLite store — same interface, same
// semantics — so nothing above the seam changes when the deployment grows.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/store"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the Postgres implementation of store.Store, review.Store,
// budget.Store, deadletter.Store and store.Leases.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Open connects to dsn (a libpq URL or key/value string), verifies the
// connection and applies pending migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Pool exposes the connection pool so the queue can share it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// SetClock overrides time for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close releases the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// migrationLockKey is the advisory lock every replica takes before migrating.
// Several orchestrator replicas start at once in Kubernetes; without it they
// would run `CREATE INDEX` against each other and one would fail its boot.
var migrationLockKey = int64(lockKey("harness.migrations"))

func lockKey(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64() >> 1) // positive: pg_advisory_lock takes a signed bigint
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var n int
		if err := conn.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = $1`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES ($1, $2)`, name, s.now().UTC()); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// CreateTask inserts t after validating it, linking it to its fan-out parent
// in the same transaction (plan Step 21).
func (s *Store) CreateTask(ctx context.Context, t *task.Task) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	doc, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tasks(id, kind, priority, requested_by, session_id, parent_id, created_at, doc)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			t.ID, string(t.Kind), t.Priority, t.RequestedBy, nullable(t.SessionID), nullable(t.ParentID), t.CreatedAt.UTC(), doc); err != nil {
			return fmt.Errorf("insert task %s: %w", t.ID, err)
		}
		if t.ParentID == "" {
			return nil
		}
		return appendChild(ctx, tx, t.ParentID, t.ID)
	})
}

func appendChild(ctx context.Context, tx pgx.Tx, parentID, childID string) error {
	var doc []byte
	// FOR UPDATE: two children of the same parent may be created concurrently
	// by two replicas, and a read-modify-write on the parent document would
	// otherwise lose one of them.
	err := tx.QueryRow(ctx, `SELECT doc FROM tasks WHERE id = $1 FOR UPDATE`, parentID).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("parent task %s: %w", parentID, store.ErrNotFound)
	}
	if err != nil {
		return err
	}
	var parent task.Task
	if err := json.Unmarshal(doc, &parent); err != nil {
		return fmt.Errorf("decode parent task %s: %w", parentID, err)
	}
	for _, c := range parent.Children {
		if c == childID {
			return nil
		}
	}
	parent.Children = append(parent.Children, childID)
	out, err := json.Marshal(&parent)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET doc = $1 WHERE id = $2`, out, parentID)
	return err
}

// GetTask loads a task by id.
func (s *Store) GetTask(ctx context.Context, id string) (*task.Task, error) {
	var doc []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM tasks WHERE id = $1`, id).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t task.Task
	if err := json.Unmarshal(doc, &t); err != nil {
		return nil, fmt.Errorf("decode task %s: %w", id, err)
	}
	return &t, nil
}

// UpdateTaskSession sets the task's resume pointer.
func (s *Store) UpdateTaskSession(ctx context.Context, taskID, sessionID string) error {
	return s.updateTask(ctx, taskID, func(t *task.Task) error {
		t.SessionID = sessionID
		return nil
	})
}

// UpdateTaskPhase records the current phase.
func (s *Store) UpdateTaskPhase(ctx context.Context, taskID string, phase int) error {
	return s.updateTask(ctx, taskID, func(t *task.Task) error {
		if t.PhaseSpec(phase) == nil {
			return fmt.Errorf("task %s has no phase %d", taskID, phase)
		}
		t.Phase = phase
		return nil
	})
}

// AdvancePhase compare-and-swaps the task's phase (fan-in, plan Step 21).
func (s *Store) AdvancePhase(ctx context.Context, taskID string, from, to int) (bool, error) {
	won := false
	err := s.updateTask(ctx, taskID, func(t *task.Task) error {
		if t.Phase != from {
			return nil
		}
		if t.PhaseSpec(to) == nil {
			return fmt.Errorf("task %s has no phase %d", taskID, to)
		}
		t.Phase = to
		won = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return won, nil
}

// ListChildTasks returns a parent's fan-out children in creation order.
func (s *Store) ListChildTasks(ctx context.Context, parentID string) ([]*task.Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM tasks WHERE parent_id = $1 ORDER BY created_at, id`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*task.Task
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var t task.Task
		if err := json.Unmarshal(doc, &t); err != nil {
			return nil, fmt.Errorf("decode child of %s: %w", parentID, err)
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// ListFanOutParents returns every task id that has at least one child.
func (s *Store) ListFanOutParents(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT parent_id FROM tasks WHERE parent_id IS NOT NULL ORDER BY parent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListTerminalTasks returns tasks whose newest run is terminal and finished
// before the cutoff (session retention sweep).
func (s *Store) ListTerminalTasks(ctx context.Context, finishedBefore time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT task_id FROM (
			SELECT task_id, status, finished_at, ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY created_at DESC, id DESC) AS rn FROM runs
		) latest WHERE rn = 1 AND status = ANY($1) AND finished_at IS NOT NULL AND finished_at < $2 ORDER BY finished_at`,
		[]string{string(task.StatusDelivered), string(task.StatusDead), string(task.StatusClosed)}, finishedBefore.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) updateTask(ctx context.Context, id string, mutate func(*task.Task) error) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var doc []byte
		err := tx.QueryRow(ctx, `SELECT doc FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(&doc)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		var t task.Task
		if err := json.Unmarshal(doc, &t); err != nil {
			return err
		}
		if err := mutate(&t); err != nil {
			return err
		}
		out, err := json.Marshal(&t)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE tasks SET session_id = $1, doc = $2 WHERE id = $3`, nullable(t.SessionID), out, id)
		return err
	})
}

// CreateRun inserts r after validating it.
func (s *Store) CreateRun(ctx context.Context, r *task.Run) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	if r.Artifacts == nil {
		r.Artifacts = []string{}
	}
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO runs(id, task_id, attempt, session_id, session_mode, status, created_at, started_at, finished_at, doc)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			r.ID, r.TaskID, r.Attempt, r.SessionID, string(r.SessionMode), string(r.Status), r.CreatedAt.UTC(), utcPtr(r.StartedAt), utcPtr(r.FinishedAt), doc); err != nil {
			return fmt.Errorf("insert run %s: %w", r.ID, err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_events(run_id, seq, at, from_status, to_status, reason) VALUES ($1, 0, $2, NULL, $3, 'created')`,
			r.ID, s.now().UTC(), string(r.Status))
		return err
	})
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (*task.Run, error) {
	var doc []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM runs WHERE id = $1`, id).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeRun(doc)
}

func decodeRun(doc []byte) (*task.Run, error) {
	var r task.Run
	if err := json.Unmarshal(doc, &r); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	return &r, nil
}

// UpdateRunStatus enforces task.Transition inside one transaction.
func (s *Store) UpdateRunStatus(ctx context.Context, runID string, to task.RunStatus, reason string) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error {
		if err := task.Transition(r.Status, to); err != nil {
			return err
		}
		now := s.now()
		r.Status = to
		if reason != "" {
			r.LastError = reason
		}
		if to == task.StatusRunning {
			n := now
			r.StartedAt = &n
			r.FinishedAt = nil
		}
		if to.Terminal() || to == task.StatusFailed || to == task.StatusPassed || to == task.StatusNeedsReview {
			n := now
			r.FinishedAt = &n
		}
		return nil
	}, reason)
}

func (s *Store) updateRun(ctx context.Context, id string, mutate func(*task.Run) error, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var doc []byte
		err := tx.QueryRow(ctx, `SELECT doc FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&doc)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		r, err := decodeRun(doc)
		if err != nil {
			return err
		}
		before := r.Status
		if err := mutate(r); err != nil {
			return err
		}
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status = $1, started_at = $2, finished_at = $3, doc = $4 WHERE id = $5`,
			string(r.Status), utcPtr(r.StartedAt), utcPtr(r.FinishedAt), out, id); err != nil {
			return err
		}
		if before == r.Status {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO run_events(run_id, seq, at, from_status, to_status, reason)
			VALUES ($1, (SELECT COALESCE(MAX(seq), -1) + 1 FROM run_events WHERE run_id = $1), $2, $3, $4, $5)`,
			id, s.now().UTC(), string(before), string(r.Status), reason)
		return err
	})
}

// ListRunsByStatus returns runs in creation order.
func (s *Store) ListRunsByStatus(ctx context.Context, status task.RunStatus) ([]*task.Run, error) {
	return s.listRuns(ctx, `SELECT doc FROM runs WHERE status = $1 ORDER BY created_at, id`, string(status))
}

// ListRunsByTask returns a task's runs in attempt order.
func (s *Store) ListRunsByTask(ctx context.Context, taskID string) ([]*task.Run, error) {
	return s.listRuns(ctx, `SELECT doc FROM runs WHERE task_id = $1 ORDER BY attempt, created_at`, taskID)
}

// LatestRun returns the newest run of a task.
func (s *Store) LatestRun(ctx context.Context, taskID string) (*task.Run, error) {
	runs, err := s.listRuns(ctx, `SELECT doc FROM runs WHERE task_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, taskID)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, store.ErrNotFound
	}
	return runs[0], nil
}

// CountAttempts returns how many runs exist for the task.
func (s *Store) CountAttempts(ctx context.Context, taskID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runs WHERE task_id = $1`, taskID).Scan(&n)
	return n, err
}

func (s *Store) listRuns(ctx context.Context, q string, args ...any) ([]*task.Run, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*task.Run
	for rows.Next() {
		var doc []byte
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		r, err := decodeRun(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordWorker stores where the run executed.
func (s *Store) RecordWorker(ctx context.Context, runID string, w task.WorkerInfo) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error { r.Worker = w; return nil }, "")
}

// RecordMetrics stores the run's metrics.
func (s *Store) RecordMetrics(ctx context.Context, runID string, m task.Metrics) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error { r.Metrics = m; return nil }, "")
}

// RecordEval stores the evaluation result.
func (s *Store) RecordEval(ctx context.Context, runID string, e task.Eval) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error { r.Eval = &e; return nil }, "")
}

// RecordOutput stores the run's structured output.
func (s *Store) RecordOutput(ctx context.Context, runID string, output json.RawMessage) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error {
		if len(output) > 0 && !json.Valid(output) {
			return fmt.Errorf("run %s output is not valid JSON", runID)
		}
		r.Output = append(json.RawMessage(nil), output...)
		return nil
	}, "")
}

// RecordArtifacts stores delivery artifacts and log/session pointers.
func (s *Store) RecordArtifacts(ctx context.Context, runID string, eventLogURI, sessionURI string, artifacts []string) error {
	return s.updateRun(ctx, runID, func(r *task.Run) error {
		if eventLogURI != "" {
			r.EventLogURI = eventLogURI
		}
		if sessionURI != "" {
			r.SessionURI = sessionURI
		}
		for _, a := range artifacts {
			dup := false
			for _, e := range r.Artifacts {
				if e == a {
					dup = true
				}
			}
			if !dup {
				r.Artifacts = append(r.Artifacts, a)
			}
		}
		return nil
	}, "")
}

// inTx runs fn in a transaction, rolling back on any error.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

// nullable maps an empty string to SQL NULL so indexed columns stay sparse.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

var _ store.Store = (*Store)(nil)

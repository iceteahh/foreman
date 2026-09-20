// Package sqlite implements store.Store on modernc.org/sqlite (pure Go, no cgo).
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver

	"github.com/100xteam-ai/foreman/internal/store"
	"github.com/100xteam-ai/foreman/internal/task"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the SQLite implementation.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path, enables WAL and
// foreign keys, and applies pending migrations. Use ":memory:" for tests
// that do not need a file; the pool is pinned to one connection so the
// in-memory database is shared.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite allows one writer; a single connection avoids SQLITE_BUSY between
	// goroutines and makes the shared in-memory DB safe.
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.ExecContext(context.Background(), p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the connection so the queue can share the same file.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
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
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES (?, ?)`, name, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// sqlTime is RFC3339 with a **fixed-width** nanosecond fraction. time.RFC3339Nano
// trims trailing zeros, which makes the fraction variable width — and these
// timestamps are compared as TEXT by SQLite, so a trimmed one sorts wrong:
// "…53.000327Z" > "…53.000327852Z", because 'Z' > '8'. A job enqueued a few
// microseconds earlier than the lease then fails `available_at <= now` and
// goes invisible until the clock moves on. Parsing still accepts the old
// variable-width rows (time.Parse ignores fraction width).
const sqlTime = "2006-01-02T15:04:05.000000000Z07:00"

func ts(t time.Time) string { return t.UTC().Format(sqlTime) }

func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// CreateTask inserts t after validating it.
func (s *Store) CreateTask(ctx context.Context, t *task.Task) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	doc, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id, kind, priority, requested_by, session_id, parent_id, created_at, doc) VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, string(t.Kind), t.Priority, t.RequestedBy, nullable(t.SessionID), nullable(t.ParentID), ts(t.CreatedAt), string(doc)); err != nil {
		return fmt.Errorf("insert task %s: %w", t.ID, err)
	}
	// A fan-out child names its parent, and the parent names its children, in
	// the same transaction: a crash between the two would leave a child no
	// fan-in could see (or a parent waiting for a child that never existed).
	if t.ParentID != "" {
		if err := appendChild(ctx, tx, t.ParentID, t.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// appendChild adds childID to the parent task's Children list, idempotently.
func appendChild(ctx context.Context, tx *sql.Tx, parentID, childID string) error {
	var doc string
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM tasks WHERE id = ?`, parentID).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("parent task %s: %w", parentID, store.ErrNotFound)
		}
		return err
	}
	var parent task.Task
	if err := json.Unmarshal([]byte(doc), &parent); err != nil {
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
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET doc = ? WHERE id = ?`, string(out), parentID)
	return err
}

// nullable maps an empty string to SQL NULL so indexed columns stay sparse.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetTask loads a task by id.
func (s *Store) GetTask(ctx context.Context, id string) (*task.Task, error) {
	var doc string
	err := s.db.QueryRowContext(ctx, `SELECT doc FROM tasks WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t task.Task
	if err := json.Unmarshal([]byte(doc), &t); err != nil {
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

// ListChildTasks returns a parent's fan-out children in creation order.
func (s *Store) ListChildTasks(ctx context.Context, parentID string) ([]*task.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM tasks WHERE parent_id = ? ORDER BY created_at, id`, parentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*task.Task
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var t task.Task
		if err := json.Unmarshal([]byte(doc), &t); err != nil {
			return nil, fmt.Errorf("decode child of %s: %w", parentID, err)
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// ListFanOutParents returns every task id that has at least one child.
func (s *Store) ListFanOutParents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT parent_id FROM tasks WHERE parent_id IS NOT NULL ORDER BY parent_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

// AdvancePhase compare-and-swaps the task's phase; false means somebody else
// already moved it (or it was never at `from`).
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

// ListTerminalTasks returns tasks whose newest run is terminal and finished
// before the cutoff (session retention sweep).
func (s *Store) ListTerminalTasks(ctx context.Context, finishedBefore time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM (
			SELECT task_id, status, finished_at, ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY created_at DESC, id DESC) AS rn FROM runs
		) WHERE rn = 1 AND status IN (?,?,?) AND finished_at IS NOT NULL AND finished_at < ? ORDER BY finished_at`,
		string(task.StatusDelivered), string(task.StatusDead), string(task.StatusClosed), ts(finishedBefore))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var doc string
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM tasks WHERE id = ?`, id).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	var t task.Task
	if err := json.Unmarshal([]byte(doc), &t); err != nil {
		return err
	}
	if err := mutate(&t); err != nil {
		return err
	}
	out, err := json.Marshal(&t)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET session_id = ?, doc = ? WHERE id = ?`, nullable(t.SessionID), string(out), id); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateRun inserts r after validating it. The parent task must exist.
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id, task_id, attempt, session_id, session_mode, status, created_at, started_at, finished_at, doc) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TaskID, r.Attempt, r.SessionID, string(r.SessionMode), string(r.Status), ts(r.CreatedAt), tsPtr(r.StartedAt), tsPtr(r.FinishedAt), string(doc)); err != nil {
		return fmt.Errorf("insert run %s: %w", r.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_events(run_id, seq, at, from_status, to_status, reason) VALUES (?, 0, ?, NULL, ?, 'created')`,
		r.ID, ts(s.now()), string(r.Status)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (*task.Run, error) {
	var doc string
	err := s.db.QueryRowContext(ctx, `SELECT doc FROM runs WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeRun(doc)
}

func decodeRun(doc string) (*task.Run, error) {
	var r task.Run
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
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
		from := r.Status
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
		_ = from
		return nil
	}, reason)
}

// updateRun loads, mutates, and writes back a run; it also appends a run_events row
// when the status changed.
func (s *Store) updateRun(ctx context.Context, id string, mutate func(*task.Run) error, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var doc string
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM runs WHERE id = ?`, id).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
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
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, started_at = ?, finished_at = ?, doc = ? WHERE id = ?`,
		string(r.Status), tsPtr(r.StartedAt), tsPtr(r.FinishedAt), string(out), id); err != nil {
		return err
	}
	if before != r.Status {
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_events(run_id, seq, at, from_status, to_status, reason)
			VALUES (?, (SELECT COALESCE(MAX(seq), -1) + 1 FROM run_events WHERE run_id = ?), ?, ?, ?, ?)`,
			id, id, ts(s.now()), string(before), string(r.Status), reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListRunsByStatus returns runs in creation order.
func (s *Store) ListRunsByStatus(ctx context.Context, status task.RunStatus) ([]*task.Run, error) {
	return s.listRuns(ctx, `SELECT doc FROM runs WHERE status = ? ORDER BY created_at, id`, string(status))
}

// ListRunsByTask returns a task's runs in attempt order.
func (s *Store) ListRunsByTask(ctx context.Context, taskID string) ([]*task.Run, error) {
	return s.listRuns(ctx, `SELECT doc FROM runs WHERE task_id = ? ORDER BY attempt, created_at`, taskID)
}

// LatestRun returns the newest run of a task.
func (s *Store) LatestRun(ctx context.Context, taskID string) (*task.Run, error) {
	runs, err := s.listRuns(ctx, `SELECT doc FROM runs WHERE task_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, taskID)
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
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE task_id = ?`, taskID).Scan(&n)
	return n, err
}

func (s *Store) listRuns(ctx context.Context, q string, args ...any) ([]*task.Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*task.Run
	for rows.Next() {
		var doc string
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

// RecordArtifacts stores delivery artifacts and log/session pointers. Empty
// strings leave the existing pointer untouched; artifacts are appended.
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

var _ store.Store = (*Store)(nil)

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/review"
)

// SavePost implements review.Store (upsert keyed by run).
func (s *Store) SavePost(ctx context.Context, p review.Post) error {
	if p.RunID == "" || p.Channel == "" {
		return errors.New("review post needs run_id and channel")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO review_posts(run_id, task_id, channel, ref, posted_at, escalated_at, resolved_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET channel = excluded.channel, ref = excluded.ref, posted_at = excluded.posted_at,
			escalated_at = excluded.escalated_at, resolved_at = excluded.resolved_at`,
		p.RunID, p.TaskID, p.Channel, p.Ref.String(), ts(p.PostedAt), tsPtr(p.EscalatedAt), tsPtr(p.ResolvedAt))
	if err != nil {
		return fmt.Errorf("save review post %s: %w", p.RunID, err)
	}
	return nil
}

// GetPost implements review.Store.
func (s *Store) GetPost(ctx context.Context, runID string) (*review.Post, error) {
	posts, err := s.listPosts(ctx, `SELECT run_id, task_id, channel, ref, posted_at, escalated_at, resolved_at FROM review_posts WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return nil, review.ErrPostNotFound
	}
	return &posts[0], nil
}

// ListPostsForEscalation implements review.Store.
func (s *Store) ListPostsForEscalation(ctx context.Context, postedBefore time.Time) ([]review.Post, error) {
	return s.listPosts(ctx, `SELECT run_id, task_id, channel, ref, posted_at, escalated_at, resolved_at FROM review_posts
		WHERE resolved_at IS NULL AND escalated_at IS NULL AND posted_at < ? ORDER BY posted_at`, ts(postedBefore))
}

func (s *Store) listPosts(ctx context.Context, q string, args ...any) ([]review.Post, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []review.Post
	for rows.Next() {
		var p review.Post
		var ref, posted string
		var esc, res sql.NullString
		if err := rows.Scan(&p.RunID, &p.TaskID, &p.Channel, &ref, &posted, &esc, &res); err != nil {
			return nil, err
		}
		p.Ref = review.ParseRef(ref)
		p.PostedAt = parseTime(posted)
		p.EscalatedAt = parseTimePtr(esc)
		p.ResolvedAt = parseTimePtr(res)
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkEscalated implements review.Store.
func (s *Store) MarkEscalated(ctx context.Context, runID string, at time.Time) error {
	return s.markPost(ctx, `UPDATE review_posts SET escalated_at = ? WHERE run_id = ?`, at, runID)
}

// MarkResolved implements review.Store.
func (s *Store) MarkResolved(ctx context.Context, runID string, at time.Time) error {
	return s.markPost(ctx, `UPDATE review_posts SET resolved_at = ? WHERE run_id = ?`, at, runID)
}

func (s *Store) markPost(ctx context.Context, q string, at time.Time, runID string) error {
	res, err := s.db.ExecContext(ctx, q, ts(at), runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return review.ErrPostNotFound
	}
	return nil
}

// RecordDecision implements review.Store.
func (s *Store) RecordDecision(ctx context.Context, rec review.DecisionRecord) (int64, error) {
	if err := rec.Decision.Validate(); err != nil {
		return 0, fmt.Errorf("record decision: %w", err)
	}
	at := rec.Decision.At
	if at.IsZero() {
		at = s.now()
	}
	var next any
	if rec.NextRunID != "" {
		next = rec.NextRunID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO review_decisions(run_id, task_id, action, decided_by, comment, source, next_run_id, decided_at, judge_verdict, checks_failed)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		rec.Decision.RunID, rec.TaskID, string(rec.Decision.Action), rec.Decision.By, rec.Decision.Comment, rec.Decision.Source, next, ts(at), rec.JudgeVerdict, boolInt(rec.ChecksFailed))
	if err != nil {
		return 0, fmt.Errorf("record decision for %s: %w", rec.Decision.RunID, err)
	}
	return res.LastInsertId()
}

// ListDecisions implements review.Store.
func (s *Store) ListDecisions(ctx context.Context, runID string) ([]review.DecisionRecord, error) {
	return s.scanDecisions(ctx, `SELECT id, run_id, task_id, action, decided_by, comment, source, next_run_id, decided_at, judge_verdict, checks_failed
		FROM review_decisions WHERE run_id = ? ORDER BY id`, runID)
}

func (s *Store) scanDecisions(ctx context.Context, q string, args ...any) ([]review.DecisionRecord, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []review.DecisionRecord
	for rows.Next() {
		var rec review.DecisionRecord
		var action, at string
		var next sql.NullString
		var failed int
		if err := rows.Scan(&rec.ID, &rec.Decision.RunID, &rec.TaskID, &action, &rec.Decision.By, &rec.Decision.Comment, &rec.Decision.Source, &next, &at, &rec.JudgeVerdict, &failed); err != nil {
			return nil, err
		}
		rec.Decision.Action = review.Action(action)
		rec.Decision.At = parseTime(at)
		rec.NextRunID = next.String
		rec.ChecksFailed = failed != 0
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListDecisionsSince implements review.Store.
func (s *Store) ListDecisionsSince(ctx context.Context, since time.Time, limit int) ([]review.DecisionRecord, error) {
	q := `SELECT id, run_id, task_id, action, decided_by, comment, source, next_run_id, decided_at, judge_verdict, checks_failed
		FROM review_decisions WHERE decided_at >= ? ORDER BY id DESC`
	args := []any{ts(since)}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.scanDecisions(ctx, q, args...)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTime(s.String)
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var _ review.Store = (*Store)(nil)

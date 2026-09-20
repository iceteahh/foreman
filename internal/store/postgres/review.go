package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/100xteam-ai/foreman/internal/review"
)

// SavePost implements review.Store (upsert keyed by run).
func (s *Store) SavePost(ctx context.Context, p review.Post) error {
	if p.RunID == "" || p.Channel == "" {
		return errors.New("review post needs run_id and channel")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO review_posts(run_id, task_id, channel, ref, posted_at, escalated_at, resolved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (run_id) DO UPDATE SET channel = excluded.channel, ref = excluded.ref, posted_at = excluded.posted_at,
			escalated_at = excluded.escalated_at, resolved_at = excluded.resolved_at`,
		p.RunID, p.TaskID, p.Channel, p.Ref.String(), p.PostedAt.UTC(), utcPtr(p.EscalatedAt), utcPtr(p.ResolvedAt))
	if err != nil {
		return fmt.Errorf("save review post %s: %w", p.RunID, err)
	}
	return nil
}

const postSelect = `SELECT run_id, task_id, channel, ref, posted_at, escalated_at, resolved_at FROM review_posts`

// GetPost implements review.Store.
func (s *Store) GetPost(ctx context.Context, runID string) (*review.Post, error) {
	posts, err := s.listPosts(ctx, postSelect+` WHERE run_id = $1`, runID)
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
	return s.listPosts(ctx, postSelect+` WHERE resolved_at IS NULL AND escalated_at IS NULL AND posted_at < $1 ORDER BY posted_at`,
		postedBefore.UTC())
}

func (s *Store) listPosts(ctx context.Context, q string, args ...any) ([]review.Post, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []review.Post
	for rows.Next() {
		var p review.Post
		var ref string
		var esc, res *time.Time
		if err := rows.Scan(&p.RunID, &p.TaskID, &p.Channel, &ref, &p.PostedAt, &esc, &res); err != nil {
			return nil, err
		}
		p.Ref = review.ParseRef(ref)
		p.EscalatedAt, p.ResolvedAt = esc, res
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkEscalated implements review.Store.
func (s *Store) MarkEscalated(ctx context.Context, runID string, at time.Time) error {
	return s.markPost(ctx, `UPDATE review_posts SET escalated_at = $1 WHERE run_id = $2`, at, runID)
}

// MarkResolved implements review.Store.
func (s *Store) MarkResolved(ctx context.Context, runID string, at time.Time) error {
	return s.markPost(ctx, `UPDATE review_posts SET resolved_at = $1 WHERE run_id = $2`, at, runID)
}

func (s *Store) markPost(ctx context.Context, q string, at time.Time, runID string) error {
	tag, err := s.pool.Exec(ctx, q, at.UTC(), runID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
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
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO review_decisions(run_id, task_id, action, decided_by, comment, source, next_run_id, decided_at, judge_verdict, checks_failed)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		rec.Decision.RunID, rec.TaskID, string(rec.Decision.Action), rec.Decision.By, rec.Decision.Comment, rec.Decision.Source,
		nullable(rec.NextRunID), at.UTC(), rec.JudgeVerdict, rec.ChecksFailed).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record decision for %s: %w", rec.Decision.RunID, err)
	}
	return id, nil
}

const decisionSelect = `SELECT id, run_id, task_id, action, decided_by, comment, source, next_run_id, decided_at, judge_verdict, checks_failed
	FROM review_decisions`

// ListDecisions implements review.Store.
func (s *Store) ListDecisions(ctx context.Context, runID string) ([]review.DecisionRecord, error) {
	return s.scanDecisions(ctx, decisionSelect+` WHERE run_id = $1 ORDER BY id`, runID)
}

// ListDecisionsSince implements review.Store.
func (s *Store) ListDecisionsSince(ctx context.Context, since time.Time, limit int) ([]review.DecisionRecord, error) {
	q := decisionSelect + ` WHERE decided_at >= $1 ORDER BY id DESC`
	args := []any{since.UTC()}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	return s.scanDecisions(ctx, q, args...)
}

func (s *Store) scanDecisions(ctx context.Context, q string, args ...any) ([]review.DecisionRecord, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []review.DecisionRecord
	for rows.Next() {
		var rec review.DecisionRecord
		var action string
		var next *string
		if err := rows.Scan(&rec.ID, &rec.Decision.RunID, &rec.TaskID, &action, &rec.Decision.By, &rec.Decision.Comment,
			&rec.Decision.Source, &next, &rec.Decision.At, &rec.JudgeVerdict, &rec.ChecksFailed); err != nil {
			return nil, err
		}
		rec.Decision.Action = review.Action(action)
		if next != nil {
			rec.NextRunID = *next
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

var _ review.Store = (*Store)(nil)

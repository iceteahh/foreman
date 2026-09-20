// Package session snapshots and restores `claude` session transcripts so a
// fresh worker can `--resume` a session started elsewhere (design §4.4).
//
// The CLI writes one transcript at $CLAUDE_CONFIG_DIR/projects/<cwd-slug>/<session_id>.jsonl
// and looks a session up by id across every projects/*/ folder, so the slug
// used on restore does not matter (cli-contract #5, #6).
package session

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Restore when no snapshot exists for the session.
var ErrNotFound = errors.New("session: snapshot not found")

// Store is the transcript persistence seam (local files now, S3 in Step 22).
type Store interface {
	// Restore copies the snapshot of sessionID into configDir so `--resume` finds it.
	// cwd is the workspace path the CLI will run in; it only decides the slug.
	Restore(ctx context.Context, taskID, sessionID, configDir, cwd string) error
	// Snapshot copies the transcript for sessionID out of configDir. It returns
	// ErrNotFound when the CLI never wrote one (e.g. a usage error before start).
	// The returned URI identifies the snapshot (file://, s3://).
	Snapshot(ctx context.Context, taskID, sessionID, configDir string) (string, error)
	// Copy duplicates the snapshot of sessionID from one task's namespace into
	// another's, keeping the id. A fan-out child is a task of its own, so the
	// planner transcript it forks must be readable under the child's task id
	// before the worker starts (design §4.4 fork mode, plan Step 21).
	// It returns ErrNotFound when the source snapshot does not exist.
	Copy(ctx context.Context, fromTaskID, toTaskID, sessionID string) (string, error)
	// Delete removes every live snapshot for a task.
	Delete(ctx context.Context, taskID string) error
}

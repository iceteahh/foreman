// Package workspace provisions an isolated checkout per run and captures what
// the worker changed. The interface is designed for a container-backed
// implementation (plan Step 14): checks run commands through Exec, never by
// touching the filesystem directly.
package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/100xteam-ai/foreman/internal/task"
)

// ExecResult is the outcome of one command run inside the workspace.
type ExecResult struct {
	Cmd      []string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	// TimedOut is set when the context expired before the command finished.
	TimedOut bool
}

// FileStat is one row of `git diff --numstat`.
type FileStat struct {
	Path    string
	Added   int
	Deleted int
	Binary  bool
	Status  string // A M D R (from --name-status)
	OldPath string // for renames
}

// Capture is what evaluation and delivery see after the worker exits.
type Capture struct {
	BaseSHA      string
	HeadSHA      string // commit created by Commit; empty until then
	Branch       string
	Diff         string
	ChangedFiles []string
	Stats        []FileStat
}

// Workspace is one provisioned checkout.
type Workspace interface {
	Path() string
	Branch() string
	// BaseSHA is the commit the worker started from.
	BaseSHA() string
	// Exec runs argv inside the workspace with the workspace env allowlist.
	Exec(ctx context.Context, name string, args ...string) (ExecResult, error)
	// Capture stages everything (including new files) and reports the diff
	// against BaseSHA. Safe to call repeatedly.
	Capture(ctx context.Context) (*Capture, error)
	// Commit records the staged tree; no-op when nothing changed.
	Commit(ctx context.Context, message string) (sha string, err error)
	// Push publishes the bot branch to origin (force-with-lease so retries update it).
	Push(ctx context.Context) error
	// Destroy removes the checkout from disk.
	Destroy() error
}

// Manager provisions workspaces.
type Manager interface {
	Provision(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error)
	// Open reattaches to a workspace that was kept after run r finished (a
	// run waiting for human review, or a failed delivery). It returns
	// ErrNoWorkspace when the checkout is gone.
	Open(ctx context.Context, t *task.Task, r *task.Run) (Workspace, error)
}

// ErrNoWorkspace is returned by Open when the checkout no longer exists.
var ErrNoWorkspace = errors.New("workspace: checkout not found")

// BranchFor is the bot branch name for a task: harness/<task_id>.
func BranchFor(t *task.Task) string { return "harness/" + t.ID }

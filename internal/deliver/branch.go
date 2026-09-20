package deliver

import (
	"context"
	"fmt"
)

// BranchPush commits the captured tree and pushes the bot branch. It is the
// base step every git-backed adapter shares, and a complete adapter on its own
// for repos without a PR API (local bare repos in the demo).
type BranchPush struct{}

func (BranchPush) Name() string { return "branch_push" }

// Deliver implements Adapter.
func (BranchPush) Deliver(ctx context.Context, in *Input) ([]string, error) {
	sha, err := PushBranch(ctx, in)
	if err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("branch:%s@%s", in.Workspace.Branch(), sha)}, nil
}

// PushBranch commits (if needed) and pushes; returns the head sha.
func PushBranch(ctx context.Context, in *Input) (string, error) {
	sha, err := in.Workspace.Commit(ctx, CommitMessage(in))
	if err != nil {
		return "", fmt.Errorf("deliver: commit: %w", err)
	}
	if sha == "" {
		c, err := in.Workspace.Capture(ctx)
		if err != nil {
			return "", err
		}
		if c.HeadSHA == "" {
			return "", fmt.Errorf("deliver: nothing to deliver (no commit beyond base %s)", in.Workspace.BaseSHA())
		}
		sha = c.HeadSHA
	}
	if err := in.Workspace.Push(ctx); err != nil {
		return "", fmt.Errorf("deliver: %w", err)
	}
	return sha, nil
}

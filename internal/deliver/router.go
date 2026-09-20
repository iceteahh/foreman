package deliver

import (
	"context"
	"fmt"

	"github.com/100xteam-ai/foreman/internal/task"
)

// ByKind routes each task kind to the adapter that matches its deliverable: a
// change kind produces a branch or pull request, a read-only kind produces a
// report. Without this, a code_review run would reach BranchPush and fail with
// "nothing to deliver" even though it did exactly what was asked.
type ByKind struct {
	// Adapters maps a kind to its adapter.
	Adapters map[task.Kind]Adapter
	// Default handles kinds with no entry.
	Default Adapter
}

// Name implements Adapter.
func (b *ByKind) Name() string {
	if b.Default != nil {
		return "by_kind(" + b.Default.Name() + ")"
	}
	return "by_kind"
}

// For returns the adapter that will handle kind.
func (b *ByKind) For(kind task.Kind) Adapter {
	if a, ok := b.Adapters[kind]; ok {
		return a
	}
	return b.Default
}

// Deliver implements Adapter.
func (b *ByKind) Deliver(ctx context.Context, in *Input) ([]string, error) {
	a := b.For(in.Task.Kind)
	if a == nil {
		return nil, fmt.Errorf("deliver: no adapter for kind %q", in.Task.Kind)
	}
	return a.Deliver(ctx, in)
}

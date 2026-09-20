// Package audit persists the full NDJSON event stream of every run (design §7).
package audit

import (
	"context"
	"io"
)

// Writer receives one run's lines in order. Append must be safe to call
// with non-JSON lines; the sink stores whatever the CLI printed.
type Writer interface {
	Append(line []byte) error
	// URI is where the log will be readable after Close (file://, s3://).
	URI() string
	io.Closer
}

// Sink opens a Writer per run.
type Sink interface {
	Open(ctx context.Context, runID string) (Writer, error)
	// Reader opens a completed log for streaming (GET /runs/{id}/events).
	Reader(ctx context.Context, runID string) (io.ReadCloser, error)
}

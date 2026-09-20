package task

import (
	"crypto/rand"
	"time"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
)

const (
	taskPrefix = "tsk_"
	runPrefix  = "run_"
)

// NewTaskID returns a new "tsk_<ULID>" identifier.
func NewTaskID() string { return taskPrefix + newULID() }

// NewRunID returns a new "run_<ULID>" identifier.
func NewRunID() string { return runPrefix + newULID() }

// NewSessionID mints the UUIDv4 the harness passes to `claude --session-id`.
// Ids are minted once per session and never reused: the CLI refuses a
// --session-id whose transcript already exists (cli-contract #3).
func NewSessionID() string { return uuid.NewString() }

func newULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

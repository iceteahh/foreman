package runner

import (
	"context"
	osexec "os/exec"
)

// shell runs a fixed test-only command line; never used with user input.
func shell(cmdline string) (string, error) {
	out, err := osexec.CommandContext(context.Background(), "sh", "-c", cmdline).Output()
	return string(out), err
}

package checks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-shellwords"
)

// AcceptanceCommands runs acceptance.commands in the workspace; any non-zero exit fails.
// Commands are split into argv with go-shellwords and executed directly: no shell.
type AcceptanceCommands struct{}

func (AcceptanceCommands) Name() string { return "acceptance_commands" }

func (AcceptanceCommands) Run(ctx context.Context, in *Input) Result {
	cmds := in.Task.Acceptance.Commands
	if len(cmds) == 0 {
		return na("task defines no acceptance commands")
	}
	if in.Workspace == nil {
		return fail("no workspace available to run acceptance commands")
	}
	timeout := in.CommandTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	var sb strings.Builder
	failed := 0
	for _, raw := range cmds {
		parser := shellwords.NewParser()
		argv, err := parser.Parse(raw)
		switch {
		case err != nil || len(argv) == 0:
			failed++
			fmt.Fprintf(&sb, "$ %s\ncannot parse command: %v\n\n", raw, err)
			continue
		case parser.Position >= 0:
			// The parser stopped at a shell operator (; | & > <): there is no shell
			// here, so refuse rather than silently run a prefix of the command.
			failed++
			fmt.Fprintf(&sb, "$ %s\nshell operators are not supported in acceptance commands (stopped at %q); run a script instead\n\n", raw, raw[parser.Position:])
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		res, err := in.Workspace.Exec(cctx, argv[0], argv[1:]...)
		cancel()
		in.Outputs[raw] = res
		fmt.Fprintf(&sb, "$ %s\nexit=%d duration=%s", raw, res.ExitCode, res.Duration.Round(time.Millisecond))
		if res.TimedOut {
			fmt.Fprintf(&sb, " TIMED OUT after %s", timeout)
		}
		sb.WriteString("\n")
		if out := tail(res.Stdout, 4000); out != "" {
			fmt.Fprintf(&sb, "stdout:\n%s\n", out)
		}
		if out := tail(res.Stderr, 4000); out != "" {
			fmt.Fprintf(&sb, "stderr:\n%s\n", out)
		}
		if err != nil {
			failed++
			var pathErr interface{ Unwrap() error }
			if errors.As(err, &pathErr) && res.ExitCode == 0 {
				fmt.Fprintf(&sb, "error: %v\n", err)
			}
		}
		sb.WriteString("\n")
	}
	if failed > 0 {
		return fail(fmt.Sprintf("%d of %d acceptance command(s) failed\n\n%s", failed, len(cmds), strings.TrimSpace(sb.String())))
	}
	return pass(strings.TrimSpace(sb.String()))
}

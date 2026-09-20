package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/100xteam-ai/foreman/internal/events"
)

// ReplayOptions tune the timeline (plan Step 17, `harness replay <run_id>`).
type ReplayOptions struct {
	// Verbose prints assistant text and full tool inputs.
	Verbose bool
	// MaxInput bounds a rendered tool input (default 160 bytes).
	MaxInput int
	// Raw lines that did not parse are shown; set true to hide them.
	HideRaw bool
}

// Replay renders a stored NDJSON event log as a human-readable timeline: one
// line per event, tool calls named with their target, and a summary of the
// result event. It reads the same bytes the runner wrote, so it works for a
// crashed run (no result event) and for the Step 4 fixtures.
func Replay(dst io.Writer, r io.Reader, opts ReplayOptions) error {
	w := &errWriter{w: dst}
	maxInput := opts.MaxInput
	if maxInput <= 0 {
		maxInput = 160
	}
	dec := events.NewDecoder(r)
	var (
		first, last time.Time
		tools       = map[string]int{}
		toolUses    int
		turns       int
		lines       int
		result      *events.ResultEvent
		sessionID   string
		model       string
	)
	for {
		ev, err := dec.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if w.err != nil {
			return w.err
		}
		lines++
		if ev.SessionID != "" && sessionID == "" {
			sessionID = ev.SessionID
		}
		at := eventTime(ev)
		if !at.IsZero() {
			if first.IsZero() {
				first = at
			}
			last = at
		}
		stamp := "        "
		if !at.IsZero() && !first.IsZero() {
			stamp = fmt.Sprintf("%7.2fs", at.Sub(first).Seconds())
		}
		switch ev.Type {
		case events.TypeSystem:
			switch ev.System.Subtype {
			case events.SubtypeInit:
				model = ev.System.Model
				w.printf("%s  init      model=%s permission=%s tools=%d cwd=%s\n",
					stamp, ev.System.Model, ev.System.PermissionMode, len(ev.System.Tools), ev.System.CWD)
			case events.SubtypeAPIRetry:
				w.printf("%s  api_retry attempt %d/%d status=%d %s (retry in %dms)\n",
					stamp, ev.System.Attempt, ev.System.MaxRetries, ev.System.ErrorStatus, ev.System.Error, ev.System.RetryDelayMS)
			case events.SubtypeStatus:
				w.printf("%s  status    %s\n", stamp, ev.System.Status)
			default:
				if opts.Verbose {
					w.printf("%s  system    %s\n", stamp, ev.System.Subtype)
				}
			}
		case events.TypeAssistant:
			turns++
			if m := ev.Assistant.Message.Model; m != "" && m != "<synthetic>" {
				model = m
			}
			for _, c := range ev.Assistant.Message.Content {
				switch c.Type {
				case "tool_use":
					toolUses++
					tools[c.Name]++
					w.printf("%s  tool      %s %s\n", stamp, c.Name, toolTarget(c, maxInput, opts.Verbose))
				case "text":
					if opts.Verbose && strings.TrimSpace(c.Text) != "" {
						w.printf("%s  assistant %s\n", stamp, clipLine(c.Text, 400))
					}
				case "thinking":
					if opts.Verbose {
						w.printf("%s  thinking  (%d chars)\n", stamp, len(c.Text))
					}
				}
			}
			if u := ev.Assistant.Message.Usage; opts.Verbose && (u.InputTokens > 0 || u.OutputTokens > 0) {
				w.printf("%s  usage     in=%d out=%d cache_read=%d cache_write=%d\n",
					stamp, u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
			}
		case events.TypeRateLimit:
			w.printf("%s  ratelimit status=%s type=%s\n", stamp, ev.RateLimit.Info.Status, ev.RateLimit.Info.RateLimitType)
		case events.TypeResult:
			result = ev.Result
		case events.TypeRaw:
			if !opts.HideRaw {
				w.printf("%s  raw       %s\n", stamp, clipLine(string(ev.Raw), 200))
			}
		default:
			if opts.Verbose {
				w.printf("%s  %-9s %s\n", stamp, ev.Type, clipLine(string(ev.Raw), 120))
			}
		}
	}

	w.printf("\n%d events", lines)
	if sessionID != "" {
		w.printf(", session %s", sessionID)
	}
	if model != "" {
		w.printf(", model %s", model)
	}
	if !first.IsZero() && last.After(first) {
		w.printf(", %.1fs of stream", last.Sub(first).Seconds())
	}
	w.printf("\n%d assistant messages, %d tool calls", turns, toolUses)
	if len(tools) > 0 {
		w.printf(" (%s)", summariseTools(tools))
	}
	w.println("")
	if result == nil {
		w.println("no result event: the worker crashed or was killed (see the run's last_error)")
		return w.err
	}
	outcome := events.Classify(result)
	w.printf("result: %s is_error=%v subtype=%s terminal_reason=%s turns=%d cost=%.6f USD duration=%dms\n",
		outcome, result.IsError, result.Subtype, result.TerminalReason, result.NumTurns, result.TotalCostUSD, result.DurationMS)
	if len(result.PermissionDenials) > 0 {
		var names []string
		for _, d := range result.PermissionDenials {
			names = append(names, d.Tool)
		}
		w.printf("permission denials: %s\n", strings.Join(names, ", "))
	}
	if len(result.Errors) > 0 {
		w.printf("errors: %s\n", strings.Join(result.Errors, "; "))
	}
	if txt := strings.TrimSpace(result.Text()); txt != "" {
		w.printf("final message: %s\n", clipLine(txt, 600))
	}
	if len(result.StructuredOutput) > 0 {
		w.printf("structured output: %s\n", clipLine(string(result.StructuredOutput), 600))
	}
	return w.err
}

// errWriter records the first write error so the renderer can stay readable:
// a timeline is a few hundred Fprintf calls, and checking each one inline
// would bury the format strings.
type errWriter struct {
	w   io.Writer
	err error
}

// printf writes a formatted line, remembering the first failure.
func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	if _, err := fmt.Fprintf(e.w, format, args...); err != nil {
		e.err = err
	}
}

// println writes s followed by a newline.
func (e *errWriter) println(s string) { e.printf("%s\n", s) }

// eventTime reads whatever timestamp the event carries.
func eventTime(ev *events.Event) time.Time {
	if ev.Assistant != nil && ev.Assistant.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, ev.Assistant.Timestamp); err == nil {
			return t
		}
	}
	var probe struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(ev.Raw, &probe); err == nil && probe.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, probe.Timestamp); err == nil {
			return t
		}
	}
	return time.Time{}
}

// toolTarget renders the interesting part of a tool input: the path, command
// or pattern, so the timeline reads like a shell history.
func toolTarget(c events.ContentBlock, max int, verbose bool) string {
	if len(c.Input) == 0 {
		return ""
	}
	if verbose {
		return clipLine(string(c.Input), max*4)
	}
	var in map[string]any
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return clipLine(string(c.Input), max)
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "url", "notebook_path", "prompt", "description"} {
		if v, ok := in[key].(string); ok && v != "" {
			return clipLine(v, max)
		}
	}
	return clipLine(string(c.Input), max)
}

func summariseTools(tools map[string]int) string {
	type kv struct {
		name string
		n    int
	}
	var list []kv
	for k, v := range tools {
		list = append(list, kv{k, v})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && (list[j].n > list[j-1].n || (list[j].n == list[j-1].n && list[j].name < list[j-1].name)); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, fmt.Sprintf("%s×%d", e.name, e.n))
	}
	return strings.Join(parts, ", ")
}

func clipLine(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " ⏎ "), "\r", ""))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

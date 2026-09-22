package events

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../testdata/events"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeAll(t *testing.T, r io.Reader, opts ...Option) []*Event {
	t.Helper()
	d := NewDecoder(r, opts...)
	var out []*Event
	for {
		ev, err := d.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestResultFixtures(t *testing.T) {
	cases := []struct {
		file    string
		outcome Outcome
		isError bool
		subtype string
		cost    float64
		text    string
	}{
		{"result_success.json", OutcomeCompleted, false, "success", 0.0196885, "OK"},
		{"result_resumed.json", OutcomeCompleted, false, "success", 0.0042955, "PINEAPPLE"},
		{"result_forked.json", OutcomeCompleted, false, "success", 0.0192845, "PINEAPPLE"},
		{"result_budget_exhausted.json", OutcomeBudgetExhausted, true, "error_max_budget_usd", 0.07496499999999999, ""},
		// Auth failure: subtype says success, is_error says otherwise (cli-contract #12).
		{"result_not_logged_in.json", OutcomeAPIError, true, "success", 0, "Not logged in · Please run /login"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			evs := decodeAll(t, bytes.NewReader(readFixture(t, tc.file)))
			if len(evs) != 1 || evs[0].Type != TypeResult || evs[0].Result == nil {
				t.Fatalf("want one result event, got %+v", evs)
			}
			r := evs[0].Result
			if Classify(r) != tc.outcome {
				t.Errorf("Classify = %s, want %s", Classify(r), tc.outcome)
			}
			if r.IsError != tc.isError || r.Subtype != tc.subtype || r.TotalCostUSD != tc.cost || r.Text() != tc.text {
				t.Errorf("fields: is_error=%v subtype=%s cost=%v text=%q", r.IsError, r.Subtype, r.TotalCostUSD, r.Text())
			}
			if r.SessionID == "" || r.NumTurns != 1 || r.PermissionDenials == nil {
				t.Errorf("missing fields: %+v", r)
			}
			if evs[0].SessionID != r.SessionID {
				t.Error("envelope session_id differs from result session_id")
			}
		})
	}
}

func TestClassifyNilIsCrash(t *testing.T) {
	if Classify(nil) != OutcomeCrash {
		t.Error("nil result must be a crash")
	}
	// The subtype fallback: a result with no terminal_reason at all, which is
	// what a CLI predating the field emits. The pinned CLI always sets it
	// (result_max_turns.json has both), so this synthetic event is the only
	// thing keeping the branch honest.
	if got := Classify(&ResultEvent{IsError: true, Subtype: "error_max_turns"}); got != OutcomeMaxTurns {
		t.Errorf("max_turns subtype: %s", got)
	}
	if got := Classify(&ResultEvent{IsError: true, Subtype: "error_max_budget"}); got != OutcomeBudgetExhausted {
		t.Errorf("max_budget subtype: %s", got)
	}
	// And terminal_reason always wins over the subtype when both are present.
	if got := Classify(&ResultEvent{IsError: true, Subtype: "error_max_turns", TerminalReason: ReasonAPIError}); got != OutcomeAPIError {
		t.Errorf("terminal_reason must win over subtype: %s", got)
	}
	if got := Classify(&ResultEvent{IsError: true, TerminalReason: "something_new"}); got != OutcomeError {
		t.Errorf("unknown error reason: %s", got)
	}
	if got := Classify(&ResultEvent{IsError: false, TerminalReason: "something_new"}); got != OutcomeError {
		t.Errorf("unknown non-error reason must not read as completed: %s", got)
	}
}

func TestStreamFixtureSIGKILL(t *testing.T) {
	evs := decodeAll(t, bytes.NewReader(readFixture(t, "stream_sigkill_mid_tool_use.ndjson")))
	types := make([]string, 0, len(evs))
	for _, e := range evs {
		types = append(types, e.Type+"/"+e.Subtype)
	}
	want := "system/init rate_limit_event/ system/thinking_tokens system/thinking_tokens assistant/ assistant/"
	if got := strings.Join(types, " "); got != want {
		t.Errorf("event order\n got %s\nwant %s", got, want)
	}
	init := evs[0].System
	if init == nil || init.Model != "claude-haiku-4-5-20251001" || init.PermissionMode != "dontAsk" || init.ClaudeCodeVersion != "2.1.243" || len(init.Tools) == 0 {
		t.Errorf("init not decoded: %+v", init)
	}
	if evs[1].RateLimit == nil || evs[1].RateLimit.Info.Status != "allowed" {
		t.Errorf("rate limit not decoded: %+v", evs[1].RateLimit)
	}
	last := evs[len(evs)-1].Assistant
	if last == nil || last.Message.Model != "claude-haiku-4-5-20251001" || last.Message.Usage.CacheCreation.Ephemeral1h != 7776 {
		t.Fatalf("assistant not decoded: %+v", last)
	}
	uses := last.ToolUses()
	if len(uses) != 1 || uses[0].Name != "Bash" || !strings.Contains(string(uses[0].Input), "sleep 8") {
		t.Errorf("tool_use not decoded: %+v", uses)
	}
	// No result event: a crash.
	var result *ResultEvent
	for _, e := range evs {
		if e.Result != nil {
			result = e.Result
		}
	}
	if Classify(result) != OutcomeCrash {
		t.Error("stream without result must classify as crash")
	}
}

func TestDecoderTolerance(t *testing.T) {
	in := "not json at all\n\n{\"type\":\"system\",\"subtype\":\"init\"}\r\n{broken\n{\"type\":\"future_thing\",\"x\":1}\n{\"type\":\"result\",\"is_error\":false}"
	evs := decodeAll(t, strings.NewReader(in))
	if len(evs) != 5 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Type != TypeRaw || string(evs[0].Raw) != "not json at all" || evs[0].Line != 1 {
		t.Errorf("raw line: %+v", evs[0])
	}
	if evs[1].Type != TypeSystem || evs[1].Line != 3 {
		t.Errorf("system line: %+v", evs[1])
	}
	if evs[2].Type != TypeRaw {
		t.Errorf("broken json should be raw: %+v", evs[2])
	}
	if evs[3].Type != "future_thing" || !strings.Contains(string(evs[3].Raw), `"x":1`) {
		t.Errorf("unknown type must be kept: %+v", evs[3])
	}
	if evs[4].Result == nil {
		t.Error("final line without newline dropped")
	}
}

func TestDecoderMaxLine(t *testing.T) {
	long := `{"type":"assistant","pad":"` + strings.Repeat("x", 5000) + `"}`
	d := NewDecoder(strings.NewReader(long+"\n"), WithMaxLine(1024))
	if _, err := d.Next(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("want ErrLineTooLong, got %v", err)
	}
	// Big lines under the limit that exceed the bufio buffer are reassembled.
	evs := decodeAll(t, strings.NewReader(long+"\n"))
	if len(evs) != 1 || evs[0].Type != TypeAssistant || len(evs[0].Raw) != len(long) {
		t.Errorf("long line not reassembled: type=%s len=%d", evs[0].Type, len(evs[0].Raw))
	}
}

func TestAuthFailureStream(t *testing.T) {
	// Captured live 2026-09-13 with an invalid key: status → 10× api_retry(401) → synthetic assistant → result.
	b, err := os.ReadFile(filepath.Join(fixtures, "stream_auth_failed_401.ndjson"))
	if err != nil {
		t.Skip("fixture not captured yet:", err)
	}
	evs := decodeAll(t, bytes.NewReader(b))
	var retries int
	var result *ResultEvent
	for _, e := range evs {
		if e.Type == TypeSystem && e.Subtype == SubtypeAPIRetry {
			retries++
			if !e.System.IsAuthFailure() || e.System.ErrorStatus != 401 || e.System.Error != "authentication_failed" || e.System.MaxRetries != 10 {
				t.Errorf("api_retry not decoded: %+v", e.System)
			}
		}
		if e.Result != nil {
			result = e.Result
		}
	}
	if retries != 10 {
		t.Errorf("retries = %d", retries)
	}
	if result == nil || !result.IsError || Classify(result) != OutcomeAPIError || result.TotalCostUSD != 0 {
		t.Errorf("result %+v", result)
	}
	if (&SystemEvent{Subtype: SubtypeAPIRetry, ErrorStatus: 529}).IsAuthFailure() || (&SystemEvent{Subtype: SubtypeInit, ErrorStatus: 401}).IsAuthFailure() {
		t.Error("only api_retry with 401/403 is an auth failure")
	}
}

// The fixtures captured on 2026-09-15 (CLI 2.1.270) close the three open Step 4 rows.
func TestNewFixturesClassify(t *testing.T) {
	cases := []struct {
		fixture string
		outcome Outcome
		isError bool
		subtype string
		turns   int
		denials int
		hasSO   bool
	}{
		{"result_structured_output.json", OutcomeCompleted, false, "success", 3, 0, true},
		{"result_max_turns.json", OutcomeMaxTurns, true, "error_max_turns", 2, 0, false},
		{"result_permission_denial.json", OutcomeCompleted, false, "success", 3, 2, false},
		{"result_bash_safe_command_allowed.json", OutcomeCompleted, false, "success", 2, 0, false},
	}
	for _, tc := range cases {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "events", tc.fixture))
		if err != nil {
			t.Fatal(err)
		}
		ev := Parse(b, 1)
		if ev.Type != TypeResult || ev.Result == nil {
			t.Fatalf("%s: not a result", tc.fixture)
		}
		r := ev.Result
		if Classify(r) != tc.outcome || r.IsError != tc.isError || r.Subtype != tc.subtype || r.NumTurns != tc.turns || len(r.PermissionDenials) != tc.denials {
			t.Errorf("%s: outcome=%s is_error=%v subtype=%s turns=%d denials=%d", tc.fixture, Classify(r), r.IsError, r.Subtype, r.NumTurns, len(r.PermissionDenials))
		}
		if so := strings.TrimSpace(string(r.StructuredOutput)); (so != "" && so != "null") != tc.hasSO {
			t.Errorf("%s: structured_output %q", tc.fixture, so)
		}
	}
	// Denial entries carry the tool name and input; max_turns puts the reason in errors and result is null.
	b, _ := os.ReadFile(filepath.Join("..", "..", "testdata", "events", "result_permission_denial.json"))
	d := Parse(b, 1).Result.PermissionDenials
	if d[0].Tool != "Write" || d[1].Tool != "Bash" || !strings.Contains(string(d[1].Input), "rm -f hello.txt") {
		t.Errorf("%+v", d)
	}
	b, _ = os.ReadFile(filepath.Join("..", "..", "testdata", "events", "result_max_turns.json"))
	mt := Parse(b, 1).Result
	if mt.Result != nil || len(mt.Errors) != 1 || !strings.Contains(mt.Errors[0], "maximum number of turns") || mt.TerminalReason != ReasonMaxTurns {
		t.Errorf("%+v", mt)
	}
	// Whole streams decode line by line with a result at the end.
	for _, name := range []string{"stream_structured_output.ndjson", "stream_max_turns.ndjson", "stream_permission_denial.ndjson"} {
		f, err := os.Open(filepath.Join("..", "..", "testdata", "events", name))
		if err != nil {
			t.Fatal(err)
		}
		dec := NewDecoder(f)
		var last *Event
		n := 0
		for {
			ev, err := dec.Next()
			if err != nil {
				break
			}
			n++
			last = ev
		}
		_ = f.Close()
		if n < 5 || last == nil || last.Type != TypeResult {
			t.Errorf("%s: %d events, last %v", name, n, last)
		}
	}
}

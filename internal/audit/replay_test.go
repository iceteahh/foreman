package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	p, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "events", name))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReplayStructuredOutputFixture(t *testing.T) {
	var out bytes.Buffer
	if err := Replay(&out, bytes.NewReader(fixture(t, "stream_structured_output.ndjson")), ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"init      model=claude-haiku-4-5",
		"tool      Read",
		"tool      StructuredOutput",
		"13 events",
		"4 assistant messages, 2 tool calls",
		"result: completed is_error=false",
		"structured output:",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("timeline missing %q:\n%s", want, s)
		}
	}
	// Stamps are seconds since the first timestamped event, so the second tool
	// call is later than the first.
	if !strings.Contains(s, "0.87s  tool      Read") || !strings.Contains(s, "4.34s  tool      StructuredOutput") {
		t.Errorf("relative timestamps wrong:\n%s", s)
	}
}

// A killed run has no result event; replay must say so instead of pretending success.
func TestReplayCrashedRunHasNoResult(t *testing.T) {
	var out bytes.Buffer
	if err := Replay(&out, bytes.NewReader(fixture(t, "stream_sigkill_mid_tool_use.ndjson")), ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "no result event") {
		t.Errorf("crash not reported:\n%s", s)
	}
	if strings.Contains(s, "result: completed") {
		t.Errorf("crash reported as success:\n%s", s)
	}
}

func TestReplayAuthFailureAndDenials(t *testing.T) {
	var out bytes.Buffer
	if err := Replay(&out, bytes.NewReader(fixture(t, "stream_auth_failed_401.ndjson")), ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "api_retry attempt 1/10 status=401 authentication_failed") {
		t.Errorf("retries not rendered:\n%s", s)
	}
	if !strings.Contains(s, "result: api_error is_error=true") {
		t.Errorf("outcome not classified:\n%s", s)
	}

	out.Reset()
	if err := Replay(&out, bytes.NewReader(fixture(t, "stream_permission_denial.ndjson")), ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "permission denials: Write") {
		t.Errorf("denials not rendered:\n%s", s)
	}
}

func TestReplayVerboseAndRawLines(t *testing.T) {
	log := append(fixture(t, "stream_max_turns.ndjson"), []byte("not json at all\n")...)
	var out bytes.Buffer
	if err := Replay(&out, bytes.NewReader(log), ReplayOptions{Verbose: true}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "raw       not json at all") {
		t.Errorf("raw line dropped:\n%s", s)
	}
	if !strings.Contains(s, "usage     in=") {
		t.Errorf("verbose usage missing:\n%s", s)
	}
	if !strings.Contains(s, "result: max_turns") {
		t.Errorf("max_turns not classified:\n%s", s)
	}
	out.Reset()
	if err := Replay(&out, bytes.NewReader(log), ReplayOptions{HideRaw: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "not json at all") {
		t.Error("HideRaw did not hide the raw line")
	}
}

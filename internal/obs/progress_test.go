package obs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/events"
)

type fakeSink struct {
	mu    sync.Mutex
	snaps []Snapshot
}

func (f *fakeSink) Update(_ context.Context, _ string, snap Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps = append(f.snaps, snap)
	return nil
}

func (f *fakeSink) last() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.snaps) == 0 {
		return Snapshot{}
	}
	return f.snaps[len(f.snaps)-1]
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.snaps)
}

// feed replays a captured stream through the progress emitter, exactly as the
// runner's decode loop does.
func feed(t *testing.T, p *Progress, runID, fixture string) {
	t.Helper()
	path, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "events", fixture))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	dec := events.NewDecoder(f)
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		p.OnEvent(runID, ev)
	}
	// Publishing is asynchronous so a slow sink cannot stall the decode loop;
	// the assertions below are about what the sink actually received.
	p.Flush()
}

func TestProgressTracksToolsFilesAndCost(t *testing.T) {
	sink := &fakeSink{}
	p := &Progress{Sink: sink, Interval: time.Hour, Logger: quiet} // only the result publishes
	p.Start("run_01", "tsk_01", "code_fix")
	feed(t, p, "run_01", "stream_structured_output.ndjson")

	// The first event publishes immediately (the thread appears as soon as the
	// worker starts); everything else is throttled until the result event.
	if sink.count() != 2 {
		t.Fatalf("published %d times, want 2 (first event and result)", sink.count())
	}
	s := sink.last()
	if !s.Done || s.Outcome != "completed" {
		t.Errorf("snapshot not finished: %+v", s)
	}
	if s.Kind != "code_fix" || s.TaskID != "tsk_01" {
		t.Errorf("labels %+v", s)
	}
	if s.Turns != 4 || s.ToolCalls != 2 {
		t.Errorf("turns=%d tools=%d", s.Turns, s.ToolCalls)
	}
	if s.Tools["Read"] != 1 || s.Tools["StructuredOutput"] != 1 {
		t.Errorf("tool counts %v", s.Tools)
	}
	if s.Model == "" || !strings.HasPrefix(s.Model, "claude-") {
		t.Errorf("model %q", s.Model)
	}
	if s.CostUSD == 0 {
		t.Error("final cost not taken from the result event")
	}
	if s.EstCostUSD <= 0 {
		t.Error("running estimate not tracked from token usage")
	}
	if s.Text == "" {
		t.Error("final message missing")
	}
	line := s.Line()
	for _, want := range []string{"run run_01 (code_fix)", "completed", "4 turns", "2 tool calls", "Read×1"} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q: %s", want, line)
		}
	}
	// Snapshot is readable while in flight and dropped on Forget.
	if _, ok := p.Snapshot("run_01"); !ok {
		t.Error("snapshot not retained")
	}
	p.Forget("run_01")
	if _, ok := p.Snapshot("run_01"); ok {
		t.Error("Forget did not drop the run")
	}
}

func TestProgressRecordsEditedFilesAndCommands(t *testing.T) {
	sink := &fakeSink{}
	p := &Progress{Sink: sink, Interval: time.Hour, Logger: quiet}
	p.Start("run_02", "tsk_02", "code_fix")
	// The permission-denial fixture edits a file, runs a command, and ends with
	// denials on the result event.
	feed(t, p, "run_02", "stream_permission_denial.ndjson")
	s := sink.last()
	if len(s.Denials) == 0 {
		t.Errorf("denials not surfaced: %+v", s)
	}
	line := s.Line()
	if !strings.Contains(line, "denied:") {
		t.Errorf("denials not rendered: %s", line)
	}

	// A synthetic stream proves file and command extraction.
	p2 := &Progress{Sink: sink, Interval: time.Hour, Logger: quiet}
	edit := `{"type":"assistant","message":{"model":"claude-haiku-4-5","role":"assistant","content":[
		{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/ws/src/a.go","old_string":"x","new_string":"y"}},
		{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test ./..."}},
		{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"/ws/src/a.go"}}],"usage":{"input_tokens":10,"output_tokens":5}}}`
	p2.OnEvent("run_03", events.Parse([]byte(edit), 1))
	p2.OnEvent("run_03", events.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","total_cost_usd":0.01,"num_turns":1,"result":"done","permission_denials":[]}`), 2))
	p2.Flush()
	s = sink.last()
	if len(s.Files) != 1 || s.Files[0] != "/ws/src/a.go" {
		t.Errorf("files %v (duplicates must be collapsed)", s.Files)
	}
	if len(s.Commands) != 1 || s.Commands[0] != "go test ./..." {
		t.Errorf("commands %v", s.Commands)
	}
	if !strings.Contains(s.Line(), "last command: go test ./...") {
		t.Errorf("line %s", s.Line())
	}
}

func TestProgressThrottlesAndAlwaysPublishesTheResult(t *testing.T) {
	sink := &fakeSink{}
	p := &Progress{Sink: sink, Interval: 50 * time.Millisecond, Logger: quiet}
	assistant := []byte(`{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	// Ten events in a tight loop publish at most twice (the first one, then
	// once the interval elapses), never ten times.
	for i := 0; i < 10; i++ {
		p.OnEvent("run_04", events.Parse(assistant, i+1))
	}
	p.Flush()
	if n := sink.count(); n > 2 {
		t.Errorf("published %d times despite the throttle", n)
	}
	before := sink.count()
	p.OnEvent("run_04", events.Parse([]byte(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","num_turns":10,"total_cost_usd":0.02,"result":"ok","permission_denials":[]}`), 11))
	p.Flush()
	if sink.count() != before+1 {
		t.Error("the result event must always publish")
	}
	if !sink.last().Done {
		t.Error("final snapshot not marked done")
	}
}

// A rate limit is surfaced immediately: the operator needs to know why the
// pool went quiet.
func TestProgressSurfacesRateLimit(t *testing.T) {
	sink := &fakeSink{}
	p := &Progress{Sink: sink, Interval: time.Hour, Logger: quiet}
	p.OnEvent("run_05", events.Parse([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`), 1))
	p.Flush()
	if sink.count() != 1 || !sink.last().RateLimited {
		t.Fatalf("rate limit not published: %d %+v", sink.count(), sink.last())
	}
	if !strings.Contains(sink.last().Line(), "rate limited") {
		t.Errorf("line %s", sink.last().Line())
	}
	// An allowed window is not a rate limit.
	sink2 := &fakeSink{}
	p2 := &Progress{Sink: sink2, Interval: time.Hour, Logger: quiet}
	p2.OnEvent("run_06", events.Parse([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour"}}`), 1))
	p2.Flush()
	if sink2.last().RateLimited {
		t.Error("an allowed rate-limit window must not count as throttling")
	}
}

func TestProgressWithoutSinkLogsAndIsNilSafe(t *testing.T) {
	p := &Progress{Interval: time.Nanosecond, Logger: quiet}
	p.OnEvent("run_07", events.Parse([]byte(`{"type":"assistant","message":{"model":"m","role":"assistant","content":[],"usage":{}}}`), 1))
	var nilProgress *Progress
	nilProgress.OnEvent("run_08", nil)
	p.OnEvent("run_07", nil)
}

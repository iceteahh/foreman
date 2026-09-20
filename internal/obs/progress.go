package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/100xteam-ai/foreman/internal/events"
	"github.com/100xteam-ai/foreman/internal/runner"
)

// Progress is the live per-run timeline of design §8: parsed stream events
// become tool calls, files edited and a ticking cost estimate. It implements
// runner.Progress, so it sees exactly the events the audit log records and
// never blocks the worker: rendering is throttled and the sink is called from
// the aggregator, not from the decode loop.
type Progress struct {
	// Sink receives rendered updates (the Slack thread). nil logs instead.
	Sink ProgressSink
	// Metrics records permission denials and rate limits seen on the stream.
	Metrics *Metrics
	// Interval is the minimum delay between updates for one run (default 15s).
	Interval time.Duration
	// Logger is used when Sink is nil.
	Logger *slog.Logger
	// Prices estimates the running cost from token usage, because cost only
	// appears on the result event (cli-contract #18).
	Prices runner.PriceTable
	// Kind labels the metrics.
	Kind func(runID string) string

	mu   sync.Mutex
	runs map[string]*runProgress
}

// ProgressSink publishes one run's timeline. review/slack implements it as a
// thread per run; the interface keeps a web UI or a webhook possible.
type ProgressSink interface {
	// Update publishes (or edits) the run's progress message.
	Update(ctx context.Context, runID string, snap Snapshot) error
}

// Snapshot is the rendered state of a run in flight.
type Snapshot struct {
	RunID      string
	TaskID     string
	Kind       string
	Model      string
	Turns      int
	ToolCalls  int
	Tools      map[string]int
	Files      []string
	Commands   []string
	EstCostUSD float64
	Started    time.Time
	Elapsed    time.Duration
	// Denials are tools the policy refused (design §5.1 signal).
	Denials []string
	// RateLimited is set once the stream reported throttling.
	RateLimited bool
	// Done is set after the result event.
	Done    bool
	Outcome string
	CostUSD float64
	// Text is the worker's final message once it arrives.
	Text string
}

// Line renders the snapshot as one log/Slack line.
func (s Snapshot) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s (%s)", s.RunID, s.Kind)
	if s.Done {
		fmt.Fprintf(&b, " %s", s.Outcome)
	}
	fmt.Fprintf(&b, ": %d turns, %d tool calls", s.Turns, s.ToolCalls)
	if len(s.Tools) > 0 {
		fmt.Fprintf(&b, " (%s)", summariseTools(s.Tools))
	}
	if len(s.Files) > 0 {
		fmt.Fprintf(&b, ", %d files edited: %s", len(s.Files), strings.Join(clipList(s.Files, 5), ", "))
	}
	if len(s.Commands) > 0 {
		fmt.Fprintf(&b, ", last command: %s", s.Commands[len(s.Commands)-1])
	}
	if s.Done && s.CostUSD > 0 {
		fmt.Fprintf(&b, ", cost %.4f USD", s.CostUSD)
	} else if s.EstCostUSD > 0 {
		fmt.Fprintf(&b, ", ~%.4f USD so far", s.EstCostUSD)
	}
	if s.Elapsed > 0 {
		fmt.Fprintf(&b, ", %s elapsed", s.Elapsed.Round(time.Second))
	}
	if len(s.Denials) > 0 {
		fmt.Fprintf(&b, ", denied: %s", strings.Join(s.Denials, ", "))
	}
	if s.RateLimited {
		b.WriteString(", rate limited")
	}
	return b.String()
}

// runProgress accumulates one run's stream.
type runProgress struct {
	snap     Snapshot
	tracker  *runner.CostTracker
	lastSend time.Time
	files    map[string]bool
}

func (p *Progress) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return 15 * time.Second
}

func (p *Progress) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Start registers a run so its first event is attributed correctly. Optional:
// OnEvent creates the entry on demand.
func (p *Progress) Start(runID, taskID, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensure(runID)
	rp := p.runs[runID]
	rp.snap.TaskID, rp.snap.Kind = taskID, kind
}

// ensure must be called with the lock held.
func (p *Progress) ensure(runID string) {
	if p.runs == nil {
		p.runs = map[string]*runProgress{}
	}
	if p.runs[runID] == nil {
		prices := p.Prices
		if prices.Prices == nil {
			prices = runner.DefaultPrices
		}
		p.runs[runID] = &runProgress{
			snap:    Snapshot{RunID: runID, Started: time.Now(), Tools: map[string]int{}},
			tracker: runner.NewCostTracker(prices),
			files:   map[string]bool{},
		}
	}
}

// OnEvent implements runner.Progress. It is called from the decode loop, so it
// only updates counters and decides whether to publish.
func (p *Progress) OnEvent(runID string, ev *events.Event) {
	if p == nil || ev == nil {
		return
	}
	p.mu.Lock()
	p.ensure(runID)
	rp := p.runs[runID]
	publish := false
	switch ev.Type {
	case events.TypeSystem:
		if ev.System.Subtype == events.SubtypeInit && ev.System.Model != "" {
			rp.snap.Model = ev.System.Model
		}
		if ev.System.IsRateLimited() {
			rp.snap.RateLimited = true
			publish = true
		}
	case events.TypeRateLimit:
		if ev.RateLimit.Limited() {
			rp.snap.RateLimited = true
			publish = true
		}
	case events.TypeAssistant:
		rp.snap.Turns++
		if m := ev.Assistant.Message.Model; m != "" && m != "<synthetic>" {
			rp.snap.Model = m
		}
		rp.snap.EstCostUSD = rp.tracker.Observe(ev.Assistant)
		for _, c := range ev.Assistant.ToolUses() {
			rp.snap.ToolCalls++
			rp.snap.Tools[c.Name]++
			if f := toolFile(c); f != "" && !rp.files[f] {
				rp.files[f] = true
				rp.snap.Files = append(rp.snap.Files, f)
			}
			if cmd := toolCommand(c); cmd != "" {
				rp.snap.Commands = append(rp.snap.Commands, cmd)
			}
		}
	case events.TypeResult:
		rp.snap.Done = true
		rp.snap.Outcome = string(events.Classify(ev.Result))
		rp.snap.CostUSD = ev.Result.TotalCostUSD
		rp.snap.Text = clipLine(ev.Result.Text(), 300)
		for _, d := range ev.Result.PermissionDenials {
			rp.snap.Denials = append(rp.snap.Denials, d.Tool)
		}
		publish = true
	}
	rp.snap.Elapsed = time.Since(rp.snap.Started)
	// The first event always publishes, so the timeline appears as soon as the
	// worker starts; after that updates are throttled to Interval, and the
	// result event always publishes.
	if !publish && time.Since(rp.lastSend) >= p.interval() {
		publish = true
	}
	var snap Snapshot
	if publish {
		rp.lastSend = time.Now()
		snap = rp.snapshot()
	}
	kind := rp.snap.Kind
	p.mu.Unlock()

	if publish {
		p.emit(runID, snap)
	}
	// Stream-level metrics are recorded once, when the result arrives.
	if ev.Type == events.TypeResult && p.Metrics != nil {
		ctx := context.Background()
		if kind == "" && p.Kind != nil {
			kind = p.Kind(runID)
		}
		for _, d := range ev.Result.PermissionDenials {
			p.Metrics.PermissionDenied(ctx, kind, d.Tool)
		}
		if snap.RateLimited {
			p.Metrics.RateLimited(ctx, kind)
		}
	}
}

// snapshot copies the state under the lock.
func (rp *runProgress) snapshot() Snapshot {
	s := rp.snap
	s.Tools = make(map[string]int, len(rp.snap.Tools))
	for k, v := range rp.snap.Tools {
		s.Tools[k] = v
	}
	s.Files = append([]string(nil), rp.snap.Files...)
	s.Commands = append([]string(nil), rp.snap.Commands...)
	s.Denials = append([]string(nil), rp.snap.Denials...)
	return s
}

func (p *Progress) emit(runID string, snap Snapshot) {
	if p.Sink == nil {
		p.log().Info("run progress", "run_id", runID, "turns", snap.Turns, "tool_calls", snap.ToolCalls,
			"files", len(snap.Files), "est_cost_usd", snap.EstCostUSD, "done", snap.Done)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Sink.Update(ctx, runID, snap); err != nil {
		p.log().Warn("publishing run progress failed", "run_id", runID, "err", err)
	}
}

// Snapshot returns a run's current state (the HTTP progress endpoint).
func (p *Progress) Snapshot(runID string) (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rp := p.runs[runID]
	if rp == nil {
		return Snapshot{}, false
	}
	return rp.snapshot(), true
}

// Forget drops a finished run's state. The pool calls it after delivery so
// long-lived processes do not accumulate snapshots.
func (p *Progress) Forget(runID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.runs, runID)
}

// toolFile extracts the file a tool call touched, for "files edited".
func toolFile(c events.ContentBlock) string {
	switch c.Name {
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return stringField(c, "file_path", "notebook_path", "path")
	}
	return ""
}

// toolCommand extracts a Bash command, for "tests run".
func toolCommand(c events.ContentBlock) string {
	if c.Name != "Bash" {
		return ""
	}
	return clipLine(stringField(c, "command"), 120)
}

func stringField(c events.ContentBlock, keys ...string) string {
	if len(c.Input) == 0 {
		return ""
	}
	var in map[string]any
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := in[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func clipList(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	out := append([]string(nil), ss[:n]...)
	return append(out, fmt.Sprintf("+%d more", len(ss)-n))
}

func summariseTools(tools map[string]int) string {
	type kv struct {
		name string
		n    int
	}
	list := make([]kv, 0, len(tools))
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
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var _ runner.Progress = (*Progress)(nil)

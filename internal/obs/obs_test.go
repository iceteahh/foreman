package obs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

// TestDisabledProviderIsInert proves the harness runs identically with no
// collector: every recorder is callable and nothing panics.
func TestDisabledProviderIsInert(t *testing.T) {
	p := Disabled()
	ctx := context.Background()
	ctx, span := p.RunSpan(ctx, "run_1", "tsk_1", "code_fix", 1, 0)
	defer span.End()
	_, st := p.Stage(ctx, StageWorker, Attr("k", "v"), AttrInt("n", 1), AttrBool("b", true), AttrFloat("f", 1.5))
	st.End()
	Fail(span, nil, "reason")
	m := p.Metrics
	m.RunFinished(ctx, "code_fix", "delivered", "completed", time.Second, 3, 0.1)
	m.Stage(ctx, "code_fix", StageChecks, time.Second)
	m.PermissionDenied(ctx, "code_fix", "Write")
	m.Retry(ctx, "code_fix", "continue")
	m.JudgeVerdict(ctx, "code_fix", "pass", false, 0.01)
	m.ReviewDecision(ctx, "code_fix", "approve", "slack", "uncertain")
	m.RateLimited(ctx, "code_fix")
	m.DeadLetter(ctx, "code_fix")
	m.WorkerStarted(ctx, "code_fix")
	m.WorkerStopped(ctx, "code_fix")
	m.Delivered(ctx, "code_fix", "github_pr", true)
	m.ObserveQueue(func() QueueSample { return QueueSample{Ready: 1} })
	m.ObserveSpend(func() []SpendSample { return []SpendSample{{Key: "global", USD: 1, Limit: 10}} })
	if TraceID(ctx) != "" {
		t.Error("the no-op tracer must not produce a trace id")
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	// A nil *Metrics is also safe: fields are optional everywhere.
	var nilMetrics *Metrics
	nilMetrics.RunFinished(ctx, "k", "s", "r", time.Second, 1, 1)
	nilMetrics.DeadLetter(ctx, "k")
}

// TestPrometheusEndpointExportsMetrics scrapes the real exporter, so the
// instrument names a dashboard queries are covered by a test.
func TestPrometheusEndpointExportsMetrics(t *testing.T) {
	addr := "127.0.0.1:19464"
	p, err := New(context.Background(), Config{Enabled: true, PrometheusAddr: addr, Environment: "test", Version: "test"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	ctx := context.Background()
	p.Metrics.ObserveQueue(func() QueueSample {
		return QueueSample{Ready: 3, Leased: 1, Dead: 2, OldestReadyAge: 42 * time.Second}
	})
	p.Metrics.ObserveSpend(func() []SpendSample {
		return []SpendSample{{Key: "code_fix", USD: 1.25, Limit: 60}}
	})
	p.Metrics.RunFinished(ctx, "code_fix", "delivered", "completed", 12*time.Second, 6, 0.073)
	p.Metrics.RunFinished(ctx, "code_fix", "dead", "max_turns", 30*time.Second, 31, 0.5)
	p.Metrics.Stage(ctx, "code_fix", StageJudge, 3*time.Second)
	p.Metrics.PermissionDenied(ctx, "code_fix", "Write")
	p.Metrics.Retry(ctx, "code_fix", "continue")
	p.Metrics.JudgeVerdict(ctx, "code_fix", "fail", true, 0.03)
	p.Metrics.ReviewDecision(ctx, "code_fix", "approve", "slack", "fail")
	p.Metrics.RateLimited(ctx, "code_fix")
	p.Metrics.DeadLetter(ctx, "code_fix")
	p.Metrics.Delivered(ctx, "code_fix", "github_pr", true)

	body := scrape(t, "http://"+addr+"/metrics")
	// The queue gauge reports the callback's values verbatim, and the ceiling
	// is a separate series so an alert can compare spend against it.
	if !strings.Contains(body, "} 42") {
		t.Errorf("queue age gauge did not report 42s:\n%s", body)
	}
	if !strings.Contains(body, "harness_budget_limit_USD{key=\"code_fix\"") {
		t.Errorf("budget ceiling is not its own series:\n%s", body)
	}
	for _, want := range []string{
		`harness_runs_total{`, `status="delivered"`, `terminal_reason="max_turns"`,
		"harness_run_duration_seconds", "harness_run_turns", "harness_run_cost_USD",
		"harness_stage_duration_seconds", `stage="judge"`,
		`harness_permission_denials_total{`, `tool="Write"`,
		`harness_retries_total{`, `session_mode="continue"`,
		`harness_judge_verdicts_total{`, `gamed_checks="true"`,
		`harness_review_decisions_total{`, `harness_review_disagreements_total{`,
		`harness_rate_limited_total{`, `harness_dead_letters_total{`, `harness_deliveries_total{`,
		`harness_queue_depth{`, `state="ready"`, `state="leased"`, `state="dead"`,
		"harness_queue_oldest_ready_age_seconds{", `otel_scope_name="harness"`,
		`harness_budget_spend_today_USD{`, `harness_budget_limit_USD{`, `key="code_fix"`,
		`service_name="harness"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics endpoint missing %q", want)
		}
	}
	// The disagreement counter fires only on a real contradiction.
	if !strings.Contains(body, `harness_review_disagreements_total{action="approve"`) {
		t.Errorf("approving a judge `fail` was not counted as a disagreement:\n%s", body)
	}
}

func TestDisagrees(t *testing.T) {
	cases := []struct {
		verdict, action string
		want            bool
	}{
		{"fail", "approve", true},
		{"fail", "reject", false},
		{"fail", "close", false},
		{"pass", "reject", true},
		{"pass", "close", true},
		{"pass", "approve", false},
		{"uncertain", "approve", false},
		{"uncertain", "reject", false},
		{"", "approve", false},
	}
	for _, c := range cases {
		if got := Disagrees(c.verdict, c.action); got != c.want {
			t.Errorf("Disagrees(%q,%q)=%v want %v", c.verdict, c.action, got, c.want)
		}
	}
}

func TestNewDisabledReturnsNoopWithoutError(t *testing.T) {
	p, err := New(context.Background(), Config{Enabled: false, OTLPEndpoint: "nowhere:4318"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if p.Registry() != nil {
		t.Error("a disabled provider must not register a Prometheus collector")
	}
	if p.Metrics == nil || p.Tracer == nil {
		t.Fatal("a disabled provider must still be usable")
	}
}

func scrape(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:noctx,gosec // local test endpoint
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("scraping %s: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

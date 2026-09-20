package obs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Stage names used for spans and for the stage-duration metric. One trace per
// run spans intake → queue → worker → checks → judge → route → deliver
// (design §8).
const (
	StageIntake  = "intake"
	StageQueue   = "queue"
	StageWorker  = "worker"
	StageChecks  = "checks"
	StageJudge   = "judge"
	StageRoute   = "route"
	StageDeliver = "deliver"
	StageReview  = "review"
)

// RunSpan starts the root span of a run. The returned context carries it, so
// stage spans nest under it automatically.
func (p *Provider) RunSpan(ctx context.Context, runID, taskID, kind string, attempt, phase int) (context.Context, trace.Span) {
	if p == nil || p.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return p.Tracer.Start(ctx, "run", trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("harness.run_id", runID),
			attribute.String("harness.task_id", taskID),
			attribute.String("harness.kind", kind),
			attribute.Int("harness.attempt", attempt),
			attribute.Int("harness.phase", phase),
		))
}

// Stage starts a child span for one pipeline stage.
func (p *Provider) Stage(ctx context.Context, stage string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if p == nil || p.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return p.Tracer.Start(ctx, stage, trace.WithAttributes(attrs...))
}

// Fail marks a span as failed with a reason; nil-safe for the no-op tracer.
func Fail(span trace.Span, err error, reason string) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	if reason != "" {
		span.SetStatus(codes.Error, reason)
	}
}

// Attr is a shorthand for a string attribute.
func Attr(k, v string) attribute.KeyValue { return attribute.String(k, v) }

// AttrInt is a shorthand for an int attribute.
func AttrInt(k string, v int) attribute.KeyValue { return attribute.Int(k, v) }

// AttrBool is a shorthand for a bool attribute.
func AttrBool(k string, v bool) attribute.KeyValue { return attribute.Bool(k, v) }

// AttrFloat is a shorthand for a float attribute.
func AttrFloat(k string, v float64) attribute.KeyValue { return attribute.Float64(k, v) }

// TraceID returns the current trace id, or "" without a recording span. It
// goes into log lines so a log can be pivoted to its trace.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// Package obs is the observability surface of design §8 and plan Step 16:
// OTel metrics for every signal the design lists, one trace per run spanning
// intake → queue → worker → checks → judge → route → deliver, and structured
// logs that always carry run_id and task_id.
//
// Everything degrades to a no-op when no exporter is configured, so the
// harness runs identically without a collector.
package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the OTel service.name of the harness.
const ServiceName = "harness"

// Config selects the exporters (harness.yaml `observability:`).
type Config struct {
	// Enabled turns the whole subsystem on. When false every recorder is a no-op.
	Enabled bool
	// OTLPEndpoint is the collector's HTTP endpoint ("localhost:4318").
	// Empty with Enabled still records metrics for the Prometheus endpoint.
	OTLPEndpoint string
	// Insecure sends OTLP over http (a local collector).
	Insecure bool
	// PrometheusAddr serves /metrics when set (":9464").
	PrometheusAddr string
	// MetricInterval is the OTLP push period (default 30s).
	MetricInterval time.Duration
	// TraceSampleRatio is the head sampling ratio (default 1: every run).
	TraceSampleRatio float64
	// Version is reported as service.version.
	Version string
	// Environment is reported as deployment.environment.
	Environment string
}

// Provider owns the SDK objects and the metric instruments.
type Provider struct {
	Config  Config
	Logger  *slog.Logger
	Tracer  trace.Tracer
	Metrics *Metrics

	meterProvider *sdkmetric.MeterProvider
	traceProvider *sdktrace.TracerProvider
	promServer    *http.Server
	registry      *prometheus.Registry
}

// Disabled is a Provider that records nothing. Every call site works with it,
// so callers never branch on whether observability is configured.
func Disabled() *Provider {
	return &Provider{Tracer: noop.NewTracerProvider().Tracer(ServiceName), Metrics: noopMetrics()}
}

// New builds the provider. It returns Disabled() (and no error) when the
// config is off, so a misconfigured collector never stops the harness.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Provider, error) {
	if !cfg.Enabled {
		return Disabled(), nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(cfg.Version),
		attribute.String("deployment.environment", cfg.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("obs resource: %w", err)
	}
	p := &Provider{Config: cfg, Logger: logger}

	var readers []sdkmetric.Option
	if cfg.PrometheusAddr != "" {
		p.registry = prometheus.NewRegistry()
		exp, err := otelprom.New(otelprom.WithRegisterer(p.registry))
		if err != nil {
			return nil, fmt.Errorf("obs prometheus exporter: %w", err)
		}
		readers = append(readers, sdkmetric.WithReader(exp))
	}
	if cfg.OTLPEndpoint != "" {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("obs otlp metric exporter: %w", err)
		}
		interval := cfg.MetricInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		readers = append(readers, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))))
	}
	mpOpts := append([]sdkmetric.Option{sdkmetric.WithResource(res)}, readers...)
	p.meterProvider = sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(p.meterProvider)

	if cfg.OTLPEndpoint != "" {
		topts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.Insecure {
			topts = append(topts, otlptracehttp.WithInsecure())
		}
		texp, err := otlptracehttp.New(ctx, topts...)
		if err != nil {
			return nil, fmt.Errorf("obs otlp trace exporter: %w", err)
		}
		ratio := cfg.TraceSampleRatio
		if ratio <= 0 {
			ratio = 1
		}
		p.traceProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(texp),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		)
		otel.SetTracerProvider(p.traceProvider)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
		p.Tracer = p.traceProvider.Tracer(ServiceName)
	} else {
		p.Tracer = noop.NewTracerProvider().Tracer(ServiceName)
	}

	m, err := NewMetrics(p.meterProvider.Meter(ServiceName))
	if err != nil {
		return nil, err
	}
	p.Metrics = m

	if cfg.PrometheusAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{}))
		p.promServer = &http.Server{Addr: cfg.PrometheusAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("prometheus metrics listening", "addr", cfg.PrometheusAddr, "path", "/metrics")
			if err := p.promServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("prometheus endpoint failed", "err", err)
			}
		}()
	}
	logger.Info("observability enabled", "otlp", cfg.OTLPEndpoint, "prometheus", cfg.PrometheusAddr,
		"trace_sample_ratio", cfg.TraceSampleRatio, "metric_interval", cfg.MetricInterval)
	return p, nil
}

// Shutdown flushes exporters. Always call it: metrics are batched.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.traceProvider != nil {
		errs = append(errs, p.traceProvider.Shutdown(ctx))
	}
	if p.meterProvider != nil {
		errs = append(errs, p.meterProvider.Shutdown(ctx))
	}
	if p.promServer != nil {
		errs = append(errs, p.promServer.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// Registry exposes the Prometheus registry (tests scrape it directly).
func (p *Provider) Registry() *prometheus.Registry { return p.registry }

// Meter returns the harness meter (or a no-op one).
func (p *Provider) Meter() metric.Meter {
	if p == nil || p.meterProvider == nil {
		return otel.GetMeterProvider().Meter(ServiceName)
	}
	return p.meterProvider.Meter(ServiceName)
}

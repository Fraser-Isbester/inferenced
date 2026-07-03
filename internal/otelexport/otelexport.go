// Package otelexport implements a telemetry.Sink that exports each metered
// inference request as an OpenTelemetry span using the GenAI semantic
// conventions, over OTLP/HTTP (protobuf, no gRPC).
//
// This is the gateway emitting its OWN authoritative, wire-parsed telemetry as
// spans — it does not ingest or trust any OTel the coding agent emits. When the
// inbound request carried a W3C traceparent (see telemetry.ParseTraceparent),
// the exported span is parented to that remote context so our ground-truth
// metering correlates with the agent's trace downstream.
package otelexport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// Config configures the OTLP/HTTP trace exporter. Zero values fall back to the
// standard OTEL_EXPORTER_OTLP_* environment variables.
type Config struct {
	// Endpoint is the OTLP/HTTP collector target: a host:port (e.g.
	// "localhost:4318") or a full URL ("http://collector:4318"). Empty uses the
	// SDK default / env.
	Endpoint string
	// Insecure disables TLS to the collector (plain HTTP), typical for a local
	// collector.
	Insecure bool
	// Headers are extra headers sent with each export (e.g. a vendor API key).
	Headers map[string]string
	// Version is the inferenced build version, recorded as service.version.
	Version string
}

// Exporter is a telemetry.Sink backed by an OTel TracerProvider.
type Exporter struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

// New builds an Exporter with a batching OTLP/HTTP span exporter. The caller
// must call Shutdown to flush buffered spans before exit.
func New(ctx context.Context, cfg Config) (*Exporter, error) {
	opts := []otlptracehttp.Option{}
	if cfg.Endpoint != "" {
		if strings.Contains(cfg.Endpoint, "://") {
			opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		} else {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		}
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}

	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel: build OTLP/HTTP exporter: %w", err)
	}

	// Merge our service identity onto the SDK's default resource. Our resource is
	// schemaless, so the merge cannot conflict on schema URL; fall back to it
	// alone on any error.
	svc := resource.NewSchemaless(
		attribute.String("service.name", "inferenced"),
		attribute.String("service.version", cfg.Version),
	)
	res, err := resource.Merge(resource.Default(), svc)
	if err != nil {
		res = svc
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	return &Exporter{tp: tp, tracer: tp.Tracer("inferenced")}, nil
}

// Emit exports one span for the event. It implements telemetry.Sink and is safe
// for concurrent use (the TracerProvider is). Span start/end come from the
// event's timestamp and duration so the span reflects the real request timing.
func (e *Exporter) Emit(ev telemetry.Event) {
	start := time.UnixMilli(ev.TimestampUnixMs)
	end := start.Add(time.Duration(ev.DurationMs) * time.Millisecond)

	ctx := context.Background()
	if sc, ok := remoteSpanContext(ev); ok {
		ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	}

	name := "chat"
	if ev.Model != "" {
		name = "chat " + ev.Model
	}

	_, span := e.tracer.Start(ctx, name,
		trace.WithTimestamp(start),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			// GenAI semantic conventions.
			attribute.String("gen_ai.system", ev.Provider),
			attribute.String("gen_ai.request.model", ev.Model),
			attribute.Int("gen_ai.usage.input_tokens", ev.InputTokens),
			attribute.Int("gen_ai.usage.output_tokens", ev.OutputTokens),
			// inferenced attributes.
			attribute.Float64("cost_usd", ev.CostUSD),
			attribute.String("developer", ev.DeveloperID),
			attribute.String("team", ev.Team),
			attribute.String("provider", ev.Provider),
			attribute.Int("http.response.status_code", ev.Status),
			attribute.Int64("duration_ms", ev.DurationMs),
			attribute.Bool("tokens_approximated", ev.TokensApproximated),
		),
	)
	span.End(trace.WithTimestamp(end))
}

// remoteSpanContext builds the parent span context from the event's captured
// W3C trace ids, so our span joins the agent's trace. It returns ok=false when
// no valid trace context was captured.
func remoteSpanContext(ev telemetry.Event) (trace.SpanContext, bool) {
	if ev.TraceID == "" || ev.SpanID == "" {
		return trace.SpanContext{}, false
	}
	tid, err := trace.TraceIDFromHex(ev.TraceID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(ev.SpanID)
	if err != nil {
		return trace.SpanContext{}, false
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}), true
}

// Shutdown flushes buffered spans and stops the exporter.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if err := e.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("otel: shutdown: %w", err)
	}
	return nil
}

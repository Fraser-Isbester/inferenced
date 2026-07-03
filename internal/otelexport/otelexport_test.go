package otelexport

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

func TestOtelexport(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OTel Export Suite")
}

// recordingExporter wires an Exporter to an in-memory SpanRecorder so specs can
// assert on the spans Emit produces without any network.
func recordingExporter() (*Exporter, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return &Exporter{tp: tp, tracer: tp.Tracer("inferenced")}, sr
}

func attrsOf(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

var _ = Describe("Exporter.Emit", func() {
	It("maps an event to a GenAI span with the expected attributes", func() {
		e, sr := recordingExporter()
		e.Emit(telemetry.Event{
			TimestampUnixMs: 1_000, DurationMs: 250,
			Provider: "anthropic", Model: "claude-sonnet-4-5",
			InputTokens: 10, OutputTokens: 5, CostUSD: 0.5,
			DeveloperID: "dev-1", Team: "platform", Status: 200,
		})

		spans := sr.Ended()
		Expect(spans).To(HaveLen(1))
		s := spans[0]
		Expect(s.Name()).To(Equal("chat claude-sonnet-4-5"))

		a := attrsOf(s.Attributes())
		Expect(a["gen_ai.system"].AsString()).To(Equal("anthropic"))
		Expect(a["gen_ai.request.model"].AsString()).To(Equal("claude-sonnet-4-5"))
		Expect(a["gen_ai.usage.input_tokens"].AsInt64()).To(Equal(int64(10)))
		Expect(a["gen_ai.usage.output_tokens"].AsInt64()).To(Equal(int64(5)))
		Expect(a["cost_usd"].AsFloat64()).To(Equal(0.5))
		Expect(a["developer"].AsString()).To(Equal("dev-1"))
		Expect(a["team"].AsString()).To(Equal("platform"))
		Expect(a[attribute.Key("http.response.status_code")].AsInt64()).To(Equal(int64(200)))
	})

	It("parents the span to the captured traceparent context", func() {
		e, sr := recordingExporter()
		e.Emit(telemetry.Event{
			TimestampUnixMs: 1_000, DurationMs: 10, Provider: "openai", Model: "gpt-4o", Status: 200,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
		})

		s := sr.Ended()[0]
		Expect(s.Parent().TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		Expect(s.Parent().SpanID().String()).To(Equal("00f067aa0ba902b7"))
		Expect(s.Parent().IsRemote()).To(BeTrue())
		// Our span shares the agent's trace id (same trace, correlated).
		Expect(s.SpanContext().TraceID().String()).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
	})

	It("emits a root span when no trace context was captured", func() {
		e, sr := recordingExporter()
		e.Emit(telemetry.Event{TimestampUnixMs: 1_000, DurationMs: 10, Provider: "ollama", Model: "llama3", Status: 200})

		s := sr.Ended()[0]
		Expect(s.Parent().IsValid()).To(BeFalse())
	})
})

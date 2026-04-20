package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestNewLoggerUsesConfiguredJSONFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "info", runtimeconfig.ObservabilityConfig{
		Logs: runtimeconfig.LogConfig{Format: "json"},
	}, LogFieldComponent, "server")
	if err != nil {
		t.Fatalf("NewLogger returned error: %v", err)
	}

	logger.Info("hello", LogFieldSubsystem, "gateway")

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("expected json log output, got error: %v\noutput=%s", err, buf.String())
	}
	if got, want := payload[LogFieldComponent], "server"; got != want {
		t.Fatalf("unexpected component: got %#v want %#v", got, want)
	}
	if got, want := payload[LogFieldSubsystem], "gateway"; got != want {
		t.Fatalf("unexpected subsystem: got %#v want %#v", got, want)
	}
}

func TestWithTraceContextAddsTraceCorrelationFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "info", runtimeconfig.ObservabilityConfig{
		Logs: runtimeconfig.LogConfig{Format: "json"},
	})
	if err != nil {
		t.Fatalf("NewLogger returned error: %v", err)
	}

	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	ctx, span := provider.Tracer("test").Start(context.Background(), "span")
	defer span.End()

	WithTraceContext(logger, ctx).Info("hello")

	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("expected json log output, got error: %v\noutput=%s", err, buf.String())
	}
	if got, want := payload[LogFieldTraceID], span.SpanContext().TraceID().String(); got != want {
		t.Fatalf("unexpected trace_id: got %#v want %#v", got, want)
	}
	if got, want := payload[LogFieldSpanID], span.SpanContext().SpanID().String(); got != want {
		t.Fatalf("unexpected span_id: got %#v want %#v", got, want)
	}
}

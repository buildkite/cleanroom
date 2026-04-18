package cli

import (
	"reflect"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestObservabilityStartupFieldsDisabled(t *testing.T) {
	t.Parallel()

	got := observabilityStartupFields(runtimeconfig.ObservabilityConfig{})
	want := []startupField{{Key: "observability", Value: "disabled"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected observability startup fields: got %#v want %#v", got, want)
	}
}

func TestObservabilityStartupFieldsOTLP(t *testing.T) {
	t.Parallel()

	ratio := 0.5
	got := observabilityStartupFields(runtimeconfig.ObservabilityConfig{
		Enabled: true,
		OTLP: runtimeconfig.OTLPConfig{
			Endpoint: "https://otel.example.test:4317",
			Protocol: "grpc",
		},
		Traces: runtimeconfig.TraceConfig{
			Sampling: runtimeconfig.TraceSamplingConfig{
				Mode:  "parentbased_traceidratio",
				Ratio: &ratio,
			},
			URLTemplate: "https://jaeger.example.test/trace/{{.TraceID}}",
		},
	})
	want := []startupField{
		{Key: "observability", Value: "enabled"},
		{Key: "trace_export", Value: "otlp/grpc -> https://otel.example.test:4317"},
		{Key: "trace_sampling", Value: "parentbased_traceidratio ratio=0.5"},
		{Key: "trace_links", Value: "enabled"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected observability startup fields: got %#v want %#v", got, want)
	}
}

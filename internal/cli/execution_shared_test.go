package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestResolveExecutionEnv(t *testing.T) {
	t.Setenv("INHERITED_ENV", "from-host")

	got, err := resolveExecutionEnv([]string{"INHERITED_ENV", "EXPLICIT=value", "EMPTY="})
	if err != nil {
		t.Fatalf("resolveExecutionEnv returned error: %v", err)
	}

	want := []string{"INHERITED_ENV=from-host", "EXPLICIT=value", "EMPTY="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected resolved env: got %v want %v", got, want)
	}
}

func TestResolveExecutionEnvRejectsUnsetInheritedVar(t *testing.T) {
	_, err := resolveExecutionEnv([]string{"UNSET_ENV_FOR_TEST"})
	if err == nil {
		t.Fatal("expected resolveExecutionEnv to fail for unset inherited variable")
	}
	if !strings.Contains(err.Error(), "UNSET_ENV_FOR_TEST") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveExecutionEnvRejectsMissingExplicitKey(t *testing.T) {
	_, err := resolveExecutionEnv([]string{"=value"})
	if err == nil {
		t.Fatal("expected resolveExecutionEnv to fail for missing key")
	}
	if !strings.Contains(err.Error(), "missing variable name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTracePreservingContextRemovesCancellationAndPreservesValues(t *testing.T) {
	rootCtx := context.WithValue(context.Background(), struct{}{}, "value")
	rootCtx = trace.ContextWithSpanContext(rootCtx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	}))
	canceledCtx, cancel := context.WithCancel(rootCtx)
	cancel()

	rpcCtx := tracePreservingContext(canceledCtx)
	if err := rpcCtx.Err(); err != nil {
		t.Fatalf("expected uncanceled context, got %v", err)
	}
	if got, want := rpcCtx.Value(struct{}{}), "value"; got != want {
		t.Fatalf("unexpected context value: got %v want %v", got, want)
	}

	gotSpanContext := trace.SpanContextFromContext(rpcCtx)
	wantSpanContext := trace.SpanContextFromContext(rootCtx)
	if gotSpanContext.TraceID() != wantSpanContext.TraceID() {
		t.Fatalf("unexpected trace id: got %s want %s", gotSpanContext.TraceID(), wantSpanContext.TraceID())
	}
	if gotSpanContext.SpanID() != wantSpanContext.SpanID() {
		t.Fatalf("unexpected span id: got %s want %s", gotSpanContext.SpanID(), wantSpanContext.SpanID())
	}
}

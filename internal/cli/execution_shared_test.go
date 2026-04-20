package cli

import (
	"bytes"
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

func TestTraceIDFromContextReturnsTraceID(t *testing.T) {
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	}))

	if got, want := traceIDFromContext(ctx), trace.SpanContextFromContext(ctx).TraceID().String(); got != want {
		t.Fatalf("unexpected trace id: got %q want %q", got, want)
	}
}

func TestExecutionCommandName(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    string
	}{
		{
			name:    "empty command",
			command: nil,
			want:    "",
		},
		{
			name:    "first non-empty arg",
			command: []string{" ", "npm", "test"},
			want:    "npm",
		},
		{
			name:    "skips leading passthrough separator",
			command: []string{"--", "echo", "hello world"},
			want:    "echo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executionCommandName(tt.command); got != tt.want {
				t.Fatalf("unexpected command name: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestExecutionCommandSummary(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    string
	}{
		{
			name:    "empty command",
			command: nil,
			want:    "",
		},
		{
			name:    "includes short command",
			command: []string{"npm", "test"},
			want:    "npm test",
		},
		{
			name:    "trims blank args and truncates long command",
			command: []string{" ", "bash", "-lc", "bundle exec rspec", "extra"},
			want:    "bash -lc bundle exec rspec ...",
		},
		{
			name:    "skips leading passthrough separator",
			command: []string{"--", "echo", "hello world"},
			want:    "echo hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executionCommandSummary(tt.command); got != tt.want {
				t.Fatalf("unexpected command summary: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestExecutionCommandArgs(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    []string
	}{
		{
			name:    "empty command",
			command: nil,
			want:    []string{},
		},
		{
			name:    "drops leading passthrough separator",
			command: []string{"--", "echo", "hello world"},
			want:    []string{"echo", "hello world"},
		},
		{
			name:    "preserves guest separator after command",
			command: []string{"--", "grep", "--", "needle", "file.txt"},
			want:    []string{"grep", "--", "needle", "file.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executionCommandArgs(tt.command); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unexpected command args: got %v want %v", got, tt.want)
			}
		})
	}
}

func TestWriteTraceID(t *testing.T) {
	var stderr bytes.Buffer

	if err := writeTraceID(&stderr, " 0123456789abcdef0123456789abcdef "); err != nil {
		t.Fatalf("writeTraceID returned error: %v", err)
	}

	if got, want := stderr.String(), "trace_id=0123456789abcdef0123456789abcdef\n"; got != want {
		t.Fatalf("unexpected trace id output: got %q want %q", got, want)
	}
}

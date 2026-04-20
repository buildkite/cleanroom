package cli

import (
	"strings"
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestNewLoggerReturnsLogFormatError(t *testing.T) {
	t.Parallel()

	_, err := newLogger("info", runtimeconfig.ObservabilityConfig{
		Logs: runtimeconfig.LogConfig{Format: "logfmt"},
	}, "server")
	if err == nil {
		t.Fatal("expected newLogger to fail for unsupported log format")
	}
	if strings.Contains(err.Error(), "invalid --log-level") {
		t.Fatalf("expected non-log-level error, got %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported observability.logs.format") {
		t.Fatalf("expected observability log format error, got %v", err)
	}
}

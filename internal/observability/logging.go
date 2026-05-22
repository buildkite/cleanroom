package observability

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"go.opentelemetry.io/otel/trace"
)

func NewLogger(w io.Writer, level string, cfg runtimeconfig.ObservabilityConfig, fields ...any) (*log.Logger, error) {
	formatter, err := resolveFormatter(cfg)
	if err != nil {
		return nil, err
	}
	parsedLevel, err := log.ParseLevel(strings.TrimSpace(level))
	if err != nil {
		return nil, err
	}
	logger := log.NewWithOptions(w, log.Options{
		Level:     parsedLevel,
		Formatter: formatter,
	})
	if len(fields) == 0 {
		return logger, nil
	}
	return logger.With(fields...), nil
}

func WithLoggerFields(logger *log.Logger, keyvals ...any) *log.Logger {
	if logger == nil {
		return nil
	}
	if len(keyvals) == 0 {
		return logger
	}
	return logger.With(keyvals...)
}

func WithTraceContext(logger *log.Logger, ctx context.Context) *log.Logger {
	if logger == nil {
		return nil
	}
	if ctx == nil {
		return logger
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return logger
	}
	return logger.With(
		LogFieldTraceID, spanContext.TraceID().String(),
		LogFieldSpanID, spanContext.SpanID().String(),
	)
}

func resolveFormatter(cfg runtimeconfig.ObservabilityConfig) (log.Formatter, error) {
	format, err := runtimeconfig.ResolveObservabilityLogFormat(cfg)
	if err != nil {
		return log.TextFormatter, err
	}
	switch format {
	case "json":
		return log.JSONFormatter, nil
	case "text":
		return log.TextFormatter, nil
	default:
		return log.TextFormatter, fmt.Errorf("unsupported log format %q", format)
	}
}

package controlservice

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (s *Service) recordCachePeerLookup(ctx context.Context, stage, direction, result string) {
	if metrics := s.serviceMetrics(); metrics != nil {
		metrics.RecordCachePeerLookup(ctx, normalizeCachePeerLookupMetricStage(stage), normalizeCachePeerLookupMetricDirection(direction), result)
	}
}

func normalizeCachePeerLookupMetricStage(stage string) string {
	switch strings.TrimSpace(stage) {
	case "":
		return "unknown"
	case dependencyStageName:
		return dependencyStageName
	case servicesStageName:
		return servicesStageName
	default:
		return "unsupported"
	}
}

func normalizeCachePeerLookupMetricDirection(direction string) string {
	switch strings.TrimSpace(direction) {
	case observability.CachePeerLookupDirectionInbound:
		return observability.CachePeerLookupDirectionInbound
	case observability.CachePeerLookupDirectionOutbound:
		return observability.CachePeerLookupDirectionOutbound
	default:
		return "unknown"
	}
}

func (s *Service) recordCachePeerImport(ctx context.Context, stage, result string) {
	if metrics := s.serviceMetrics(); metrics != nil {
		metrics.RecordCachePeerImport(ctx, stage, result)
	}
}

func (s *Service) recordCachePeerTransfer(ctx context.Context, stage, direction, result string, bytes int64, duration time.Duration) {
	if metrics := s.serviceMetrics(); metrics != nil {
		metrics.RecordCachePeerTransfer(ctx, stage, direction, result, bytes, duration)
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(observability.AttrCachePeerDirection, strings.TrimSpace(direction)),
		attribute.Int64(observability.AttrCachePeerBytes, bytes),
	)
}

func (s *Service) logCachePeerImportFallback(ctx context.Context, stage, peerURL, reason string, err error) {
	logger := observability.WithTraceContext(s.Logger, ctx)
	if logger == nil {
		return
	}
	keyvals := []any{
		"stage", strings.TrimSpace(stage),
		"peer", strings.TrimSpace(peerURL),
		"reason", strings.TrimSpace(reason),
	}
	if err != nil {
		keyvals = append(keyvals, "error", err)
	}
	logger.Debug("cache peer import fallback", keyvals...)
}

func (s *Service) logCachePeerImportCompleted(ctx context.Context, stage, peerURL string, bytes int64, duration time.Duration) {
	logger := observability.WithTraceContext(s.Logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("cache peer import completed",
		"stage", strings.TrimSpace(stage),
		"peer", strings.TrimSpace(peerURL),
		"bytes", bytes,
		"duration", duration,
	)
}

func (s *Service) logCachePeerExportCompleted(ctx context.Context, stage string, bytes int64, duration time.Duration) {
	logger := observability.WithTraceContext(s.Logger, ctx)
	if logger == nil {
		return
	}
	logger.Info("cache peer export completed",
		"stage", strings.TrimSpace(stage),
		"bytes", bytes,
		"duration", duration,
	)
}

type cachePeerCountingReadCloser struct {
	io.ReadCloser
	bytes int64
}

func (r *cachePeerCountingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	return n, err
}

type cachePeerCountingWriter struct {
	dst   io.Writer
	bytes int64
}

func (w *cachePeerCountingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.bytes += int64(n)
	return n, err
}

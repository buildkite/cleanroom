package controlservice

import (
	"context"
	"strings"

	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func cachePhaseAttributes(stage, operation string, repository *repositorycheckout.Checkout, extra ...attribute.KeyValue) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(extra)+3)
	attrs = append(attrs,
		attribute.String(observability.AttrCacheStage, strings.TrimSpace(stage)),
		attribute.String(observability.AttrCacheOperation, strings.TrimSpace(operation)),
	)
	if repository != nil {
		if commitSHA := strings.TrimSpace(repository.CommitSHA); commitSHA != "" {
			attrs = append(attrs, attribute.String(observability.AttrRepositoryCommitSHA, commitSHA))
		}
	}
	attrs = append(attrs, extra...)
	return attrs
}

func setCacheLookupSpanAttributes(ctx context.Context, hit bool, reason string, err error) {
	result := observability.CacheResultFailed
	if err == nil {
		if hit {
			result = observability.CacheResultHit
		} else {
			result = observability.CacheResultMiss
		}
	}
	attrs := []attribute.KeyValue{attribute.String(observability.AttrCacheResult, result)}
	if reason = strings.TrimSpace(reason); reason != "" {
		attrs = append(attrs, attribute.String(observability.AttrCacheLookupReason, reason))
	}
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}

func setCacheResultSpanAttribute(ctx context.Context, result string) {
	if result = strings.TrimSpace(result); result == "" {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCacheResult, result))
}

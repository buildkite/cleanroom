package controlservice

import (
	"context"
	"strings"

	"github.com/buildkite/cleanroom/internal/cachestore"
)

func stampCacheRecordOwner(ctx context.Context, record *cachestore.Record) {
	if record == nil {
		return
	}
	owner, ok := ownerForContext(ctx)
	if !ok {
		return
	}
	record.OwnerPrincipalID = strings.TrimSpace(owner.PrincipalID)
	record.OwnerScope = strings.TrimSpace(owner.Scope)
}

func lookupReadyCacheRecord(ctx context.Context, store cacheMetadataStore, stage, cacheKey string) (cachestore.Record, bool, error) {
	if store == nil {
		return cachestore.Record{}, false, nil
	}
	owner, ok := ownerForContext(ctx)
	if !ok {
		return store.GetReady(ctx, stage, cacheKey)
	}
	return store.GetReadyForOwner(ctx, stage, cacheKey, owner.PrincipalID)
}

func touchCacheRecord(ctx context.Context, store cacheMetadataStore, record cachestore.Record) error {
	if store == nil {
		return nil
	}
	ownerPrincipalID := strings.TrimSpace(record.OwnerPrincipalID)
	if ownerPrincipalID != "" {
		return store.TouchForOwner(ctx, record.Stage, record.CacheKey, ownerPrincipalID)
	}
	return store.Touch(ctx, record.Stage, record.CacheKey)
}

func deleteCacheRecord(ctx context.Context, store cacheMetadataStore, record cachestore.Record) error {
	if store == nil {
		return nil
	}
	return store.DeleteForOwner(ctx, record.Stage, record.CacheKey, strings.TrimSpace(record.OwnerPrincipalID))
}

func cacheRecordVisibleToContext(ctx context.Context, record cachestore.Record) bool {
	owner, ok := ownerForContext(ctx)
	recordOwner := strings.TrimSpace(record.OwnerPrincipalID)
	if !ok {
		return recordOwner == ""
	}
	return recordOwner != "" && recordOwner == strings.TrimSpace(owner.PrincipalID)
}

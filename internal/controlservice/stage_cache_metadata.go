package controlservice

import (
	"runtime"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
)

func populateStageCacheRecordMetadata(record *cachestore.Record, runtimeBaseKey string, result *backend.SnapshotResult, validatedAt time.Time) {
	if record == nil {
		return
	}
	record.Architecture = runtime.GOARCH
	record.RuntimeBaseKey = strings.TrimSpace(runtimeBaseKey)
	if !validatedAt.IsZero() {
		record.LastValidatedAt = validatedAt.UTC()
	}
	if result == nil {
		return
	}
	record.StorageSizeBytes = result.StorageSizeBytes
	record.ExclusiveSizeBytes = result.ExclusiveSizeBytes
	record.DriverMetadata = strings.TrimSpace(result.DriverMetadata)
}

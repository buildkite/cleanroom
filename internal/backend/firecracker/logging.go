package firecracker

import (
	"strings"

	charmlog "charm.land/log/v2"
	"github.com/buildkite/cleanroom/internal/observability"
)

func baseFirecrackerLogger(logger *charmlog.Logger) *charmlog.Logger {
	if logger != nil {
		return logger
	}
	return charmlog.Default().With(observability.LogFieldBackend, "firecracker")
}

func logPersistentVolumeCleanup(logger *charmlog.Logger, volumeRef string, err error) {
	if err == nil {
		return
	}
	observability.WithLoggerFields(baseFirecrackerLogger(logger), "volume_ref", strings.TrimSpace(volumeRef)).Warn("cleanup persistent volume", "error", err)
}

func logExecutionNotice(logger *charmlog.Logger, runID, notice string) {
	msg := strings.TrimSpace(notice)
	if msg == "" {
		return
	}
	entry := baseFirecrackerLogger(logger)
	if id := strings.TrimSpace(runID); id != "" {
		entry = entry.With(observability.LogFieldExecutionID, id)
	}
	entry.Info(msg)
}

package controlservice

import (
	"fmt"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
)

func blockVolumeEscapedWriteWarning(stage, blockName string, result *backend.ExecutionResult) string {
	if result == nil || result.OverlayCapture == nil || len(result.OverlayCapture.EscapedWrites) == 0 {
		return ""
	}
	count := len(result.OverlayCapture.EscapedWrites)
	return fmt.Sprintf("%s block %q wrote outside declared outputs (%d escaped writes: %s); skipping block-volume cache publication", stage, blockName, count, blockVolumeEscapedWriteSummary(result.OverlayCapture.EscapedWrites))
}

func blockVolumeEscapedWriteSummary(entries []backend.OverlayCaptureEntry) string {
	const limit = 3
	paths := make([]string, 0, min(len(entries), limit))
	for i, entry := range entries {
		if i >= limit {
			break
		}
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			path = "<unknown>"
		}
		paths = append(paths, path)
	}
	if len(entries) > limit {
		paths = append(paths, fmt.Sprintf("+%d more", len(entries)-limit))
	}
	return strings.Join(paths, ", ")
}

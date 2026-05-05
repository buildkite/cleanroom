package controlservice

import (
	"path/filepath"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

const blockVolumeOverlayCaptureRoot = "/run/cleanroom/overlay-captures"

var blockVolumeOverlayCaptureIgnoredPrefixes = []string{"/tmp", "/var/tmp", "/run"}

func blockVolumeOverlayCapture(stage, cacheKey string, outputs policy.StageBlockOutputs) *backend.OverlayCapture {
	return &backend.OverlayCapture{
		UpperDir:            filepath.Join(blockVolumeOverlayCaptureRoot, blockVolumeID(stage, cacheKey), "upper"),
		BaselinePaths:       append([]string(nil), outputs.Dirs...),
		DeclaredFileOutputs: append([]string(nil), outputs.Files...),
		IgnoredPrefixes:     append([]string(nil), blockVolumeOverlayCaptureIgnoredPrefixes...),
	}
}

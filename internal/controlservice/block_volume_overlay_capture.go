package controlservice

import (
	"path/filepath"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

const blockVolumeOverlayCaptureRoot = "/run/cleanroom/overlay-captures"

var blockVolumeOverlayCaptureIgnoredPrefixes = []string{"/tmp", "/var/tmp", "/run"}

func blockVolumeOverlayCapture(stage, cacheKey string, outputs policy.StageBlockOutputs, priorOutputDirs []string) *backend.OverlayCapture {
	return &backend.OverlayCapture{
		UpperDir:            filepath.Join(blockVolumeOverlayCaptureRoot, blockVolumeID(stage, cacheKey), "upper"),
		BaselinePaths:       blockVolumeOverlayBaselinePaths(priorOutputDirs, outputs.Dirs),
		DeclaredFileOutputs: append([]string(nil), outputs.Files...),
		IgnoredPrefixes:     append([]string(nil), blockVolumeOverlayCaptureIgnoredPrefixes...),
	}
}

func blockVolumeOverlayBaselinePaths(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, group := range groups {
		for _, path := range group {
			if path == "" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			out = append(out, path)
		}
	}
	return out
}

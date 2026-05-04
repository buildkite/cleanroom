//go:build linux

package main

import (
	"fmt"

	"github.com/buildkite/cleanroom/internal/overlaycapture"
	"github.com/buildkite/cleanroom/internal/vsockexec"
)

func scanOverlayCapture(capture *vsockexec.OverlayCapture) (*vsockexec.OverlayCaptureResult, error) {
	if capture == nil {
		return nil, nil
	}
	result, err := overlaycapture.Scan(capture.UpperDir, overlaycapture.Options{
		BaselinePaths:       append([]string(nil), capture.BaselinePaths...),
		DeclaredFileOutputs: append([]string(nil), capture.DeclaredFileOutputs...),
		IgnoredPrefixes:     append([]string(nil), capture.IgnoredPrefixes...),
	})
	if err != nil {
		return nil, fmt.Errorf("scan overlay capture: %w", err)
	}
	return &vsockexec.OverlayCaptureResult{
		Entries:       overlayCaptureEntries(result.Entries),
		EscapedWrites: overlayCaptureEntries(result.EscapedWrites),
	}, nil
}

func overlayCaptureEntries(entries []overlaycapture.Entry) []vsockexec.OverlayCaptureEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]vsockexec.OverlayCaptureEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, vsockexec.OverlayCaptureEntry{
			Path: entry.Path,
			Kind: string(entry.Kind),
			Mode: uint32(entry.Mode),
		})
	}
	return out
}

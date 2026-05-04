//go:build !linux

package overlaycapture

import "io/fs"

func overlayEntryIsWhiteout(string, fs.FileInfo) bool {
	return false
}

func overlayDirIsOpaque(string) bool {
	return false
}

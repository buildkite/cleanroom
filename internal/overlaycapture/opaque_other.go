//go:build !linux

package overlaycapture

func overlayDirIsOpaque(string) bool {
	return false
}

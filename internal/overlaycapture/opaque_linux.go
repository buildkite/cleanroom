//go:build linux

package overlaycapture

import "golang.org/x/sys/unix"

func overlayDirIsOpaque(path string) bool {
	value := make([]byte, 1)
	n, err := unix.Getxattr(path, "trusted.overlay.opaque", value)
	return err == nil && n == 1 && value[0] == 'y'
}

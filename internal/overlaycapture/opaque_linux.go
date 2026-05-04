//go:build linux

package overlaycapture

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

func overlayEntryIsWhiteout(path string, info fs.FileInfo) bool {
	if info.Mode()&fs.ModeCharDevice != 0 {
		var stat unix.Stat_t
		if err := unix.Lstat(path, &stat); err != nil {
			return false
		}
		return unix.Major(uint64(stat.Rdev)) == 0 && unix.Minor(uint64(stat.Rdev)) == 0
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return false
	}
	return hasOverlayXattr(path, "trusted.overlay.whiteout") || hasOverlayXattr(path, "user.overlay.whiteout")
}

func overlayDirIsOpaque(path string) bool {
	return overlayXattrValueIn(path, "trusted.overlay.opaque", 'y', 'x') ||
		overlayXattrValueIn(path, "user.overlay.opaque", 'y', 'x')
}

func hasOverlayXattr(path, name string) bool {
	_, err := unix.Getxattr(path, name, nil)
	return err == nil || (err != nil && !xattrMissing(err) && overlayXattrValueIn(path, name, 0))
}

func overlayXattrValueIn(path, name string, allowed ...byte) bool {
	value := make([]byte, 1)
	n, err := unix.Getxattr(path, name, value)
	if err != nil || n != 1 {
		return false
	}
	for _, want := range allowed {
		if want == 0 || value[0] == want {
			return true
		}
	}
	return false
}

func xattrMissing(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

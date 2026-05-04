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
	return overlayXattrValueIs(path, "trusted.overlay.opaque", 'y') || overlayXattrValueIs(path, "user.overlay.opaque", 'y')
}

func hasOverlayXattr(path, name string) bool {
	_, err := unix.Getxattr(path, name, nil)
	return err == nil || (err != nil && !xattrMissing(err) && overlayXattrValueIs(path, name, 0))
}

func overlayXattrValueIs(path, name string, want byte) bool {
	value := make([]byte, 1)
	n, err := unix.Getxattr(path, name, value)
	return err == nil && n == 1 && (want == 0 || value[0] == want)
}

func xattrMissing(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

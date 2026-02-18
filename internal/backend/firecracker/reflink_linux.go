//go:build linux

package firecracker

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryCloneFile(dst, src *os.File) bool {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}

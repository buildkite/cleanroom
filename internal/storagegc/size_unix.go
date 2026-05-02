//go:build darwin || linux

package storagegc

import (
	"os"
	"syscall"
)

func allocatedFileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Blocks >= 0 {
		return int64(stat.Blocks) * 512
	}
	return info.Size()
}

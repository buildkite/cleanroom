//go:build !darwin && !linux

package volumestore

import "os"

func allocatedFileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

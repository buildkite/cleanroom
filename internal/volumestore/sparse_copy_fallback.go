//go:build !darwin && !linux

package volumestore

import "os"

func copySparseFile(dst, src *os.File, _ int64) error {
	return copyDenseFile(dst, src)
}

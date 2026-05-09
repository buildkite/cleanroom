//go:build darwin || linux

package volumestore

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func copySparseFile(dst, src *os.File, size int64) error {
	if size <= 0 {
		return dst.Truncate(0)
	}
	if err := dst.Truncate(size); err != nil {
		return err
	}

	for offset := int64(0); offset < size; {
		dataOffset, err := src.Seek(offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return nil
			}
			if sparseSeekUnsupported(err) {
				return copyDenseFile(dst, src)
			}
			return err
		}
		if dataOffset >= size {
			return nil
		}

		holeOffset, err := src.Seek(dataOffset, unix.SEEK_HOLE)
		if err != nil {
			if sparseSeekUnsupported(err) {
				return copyDenseFile(dst, src)
			}
			return err
		}
		if holeOffset > size {
			holeOffset = size
		}
		if holeOffset <= dataOffset {
			return copyDenseFile(dst, src)
		}

		if _, err := src.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}
		if _, err := dst.Seek(dataOffset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, holeOffset-dataOffset); err != nil {
			return err
		}
		offset = holeOffset
	}

	return nil
}

func sparseSeekUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.ENOTSUP)
}

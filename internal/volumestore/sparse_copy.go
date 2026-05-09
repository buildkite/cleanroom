package volumestore

import (
	"io"
	"os"
)

func copyDenseFile(dst, src *os.File) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := dst.Truncate(0); err != nil {
		return err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.Copy(dst, src)
	return err
}

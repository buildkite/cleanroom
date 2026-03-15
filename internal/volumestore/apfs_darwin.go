//go:build darwin

package volumestore

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type APFSDriverOptions struct {
	SnapshotBaseDir string
	Namespace       string
}

type APFSDriver struct {
	*pathDriver
}

func NewAPFSDriver(opts APFSDriverOptions) (*APFSDriver, error) {
	driver, err := newPathDriver("apfs", opts.SnapshotBaseDir, opts.Namespace, cloneFile)
	if err != nil {
		return nil, err
	}
	return &APFSDriver{pathDriver: driver}, nil
}

func cloneFile(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing clone target %q: %w", dst, err)
	}
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return fmt.Errorf("clonefile %q -> %q: %w", src, dst, err)
	}

	out, err := os.Open(dst)
	if err != nil {
		return fmt.Errorf("open cloned file %q: %w", dst, err)
	}
	defer out.Close()

	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync cloned file %q: %w", dst, err)
	}
	return nil
}

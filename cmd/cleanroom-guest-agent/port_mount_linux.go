//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func ensureProcMounted() error {
	if err := os.MkdirAll("/proc", 0o755); err != nil {
		return err
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if errors.Is(err, unix.EBUSY) {
			return nil
		}
		return err
	}
	return nil
}

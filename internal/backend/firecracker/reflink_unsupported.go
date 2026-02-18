//go:build !linux

package firecracker

import "os"

func tryCloneFile(dst, src *os.File) bool {
	return false
}

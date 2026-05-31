//go:build !darwin

package main

import (
	"fmt"
	"runtime"
)

func listenVsock(_ uint32) (streamListener, error) {
	return nil, fmt.Errorf("macOS guest agent vsock listener is only supported on darwin (current: %s)", runtime.GOOS)
}

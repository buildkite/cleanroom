//go:build !linux

package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr, "cleanroom-guest-agent is only supported on linux (current: %s)\n", runtime.GOOS)
	os.Exit(1)
}

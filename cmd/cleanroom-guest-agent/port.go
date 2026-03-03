package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func resolveGuestAgentPort(defaultPort uint32, envPort, cmdline string) (uint32, error) {
	rawPort := strings.TrimSpace(envPort)
	source := "CLEANROOM_VSOCK_PORT"
	if rawPort == "" {
		if cmdlinePort, ok := kernelCmdlineValue(cmdline, "cleanroom_guest_port"); ok {
			rawPort = strings.TrimSpace(cmdlinePort)
			source = "cleanroom_guest_port"
		}
	}
	if rawPort == "" {
		return defaultPort, nil
	}

	parsed, err := strconv.ParseUint(rawPort, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", source, rawPort, err)
	}
	return uint32(parsed), nil
}

func readKernelCmdline() string {
	return readKernelCmdlineWith(os.ReadFile, ensureProcMounted)
}

func readKernelCmdlineWith(readFileFn func(string) ([]byte, error), mountProcFn func() error) string {
	raw, err := readFileFn("/proc/cmdline")
	if err == nil {
		return strings.TrimSpace(string(raw))
	}
	if mountProcFn != nil {
		if mountErr := mountProcFn(); mountErr == nil {
			raw, err = readFileFn("/proc/cmdline")
		}
	}
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func kernelCmdlineValue(cmdline, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	prefix := key + "="
	for _, token := range strings.Fields(cmdline) {
		if strings.HasPrefix(token, prefix) {
			return strings.TrimPrefix(token, prefix), true
		}
	}
	return "", false
}

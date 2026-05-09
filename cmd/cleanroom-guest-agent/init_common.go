package main

import (
	"fmt"
	"os"
	"strings"
)

func appendHostsLineIfMissing(content []byte, address, hostname, line string) []byte {
	if hostsHasAddressName(string(content), address, hostname) {
		return content
	}
	next := append([]byte(nil), content...)
	if len(next) > 0 && next[len(next)-1] != '\n' {
		next = append(next, '\n')
	}
	next = append(next, line...)
	next = append(next, '\n')
	return next
}

func hostsHasAddressName(content, address, hostname string) bool {
	for _, rawLine := range strings.Split(content, "\n") {
		line, _, _ := strings.Cut(rawLine, "#")
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != address {
			continue
		}
		for _, field := range fields[1:] {
			if field == hostname {
				return true
			}
		}
	}
	return false
}

func prefixToIPv4Mask(prefix int) (string, bool) {
	if prefix < 0 || prefix > 32 {
		return "", false
	}
	octets := [4]int{}
	remaining := prefix
	for i := range octets {
		switch {
		case remaining >= 8:
			octets[i] = 255
			remaining -= 8
		case remaining > 0:
			octets[i] = 256 - (1 << (8 - remaining))
			remaining = 0
		default:
			octets[i] = 0
		}
	}
	return fmt.Sprintf("%d.%d.%d.%d", octets[0], octets[1], octets[2], octets[3]), true
}

func firstNonLoopbackInterface(names []string) string {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" && name != "lo" {
			return name
		}
	}
	return ""
}

func firstCharacterDevice(paths []string, stat func(string) (os.FileInfo, error)) string {
	for _, path := range paths {
		info, err := stat(path)
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return path
		}
	}
	return ""
}

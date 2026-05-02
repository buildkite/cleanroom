package controlservice

import (
	"fmt"
	"strings"
	"time"

	"go.jetify.com/typeid"
)

var generateTypeID = func(prefix string) (string, error) {
	id, err := typeid.WithPrefix(prefix)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func newSandboxID() string {
	return newIDWithFallbackPrefix("", "cr")
}

func newExecutionID() string {
	return newID("exec")
}

func newSnapshotID() string {
	return newID("snap")
}

func newInteractiveSessionID() string {
	return newID("isess")
}

func newID(prefix string) string {
	return newIDWithFallbackPrefix(prefix, prefix)
}

func newIDWithFallbackPrefix(prefix, fallbackPrefix string) string {
	prefix = strings.TrimSpace(prefix)
	id, err := generateTypeID(prefix)
	if err == nil && strings.TrimSpace(id) != "" {
		return id
	}

	fallbackPrefix = strings.TrimSpace(fallbackPrefix)
	timestamp := time.Now().UTC().UnixNano()
	if fallbackPrefix == "" {
		return fmt.Sprintf("%d", timestamp)
	}
	return fmt.Sprintf("%s-%d", fallbackPrefix, timestamp)
}

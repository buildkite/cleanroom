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
	return newID("cr")
}

func newExecutionID() string {
	return newID("exec")
}

func newRunID() string {
	return newID("run")
}

func newID(prefix string) string {
	id, err := generateTypeID(prefix)
	if err == nil && strings.TrimSpace(id) != "" {
		return id
	}

	return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
}

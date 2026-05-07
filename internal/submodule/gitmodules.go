package submodule

import (
	"bytes"
	"fmt"
	"strings"
)

type GitmoduleEntry struct {
	Name string
	Path string
	URL  string
}

func ParseGitmodules(data []byte) ([]GitmoduleEntry, error) {
	var entries []GitmoduleEntry
	var current *GitmoduleEntry
	seenPaths := make(map[string]struct{})

	for _, rawLine := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if current != nil {
				if err := validateAndAppend(&entries, current, seenPaths); err != nil {
					return nil, err
				}
			}
			inner := line[1 : len(line)-1]
			const prefix = `submodule "`
			if !strings.HasPrefix(inner, prefix) || !strings.HasSuffix(inner, `"`) {
				current = nil
				continue
			}
			name := inner[len(prefix) : len(inner)-1]
			current = &GitmoduleEntry{Name: name}
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "path":
			current.Path = value
		case "url":
			current.URL = value
		}
	}
	if current != nil {
		if err := validateAndAppend(&entries, current, seenPaths); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func validateAndAppend(entries *[]GitmoduleEntry, entry *GitmoduleEntry, seenPaths map[string]struct{}) error {
	if entry.Path == "" {
		return fmt.Errorf("submodule %q is missing path", entry.Name)
	}
	if entry.URL == "" {
		return fmt.Errorf("submodule %q is missing url", entry.Name)
	}
	if _, exists := seenPaths[entry.Path]; exists {
		return fmt.Errorf("duplicate submodule path %q", entry.Path)
	}
	seenPaths[entry.Path] = struct{}{}
	*entries = append(*entries, *entry)
	return nil
}

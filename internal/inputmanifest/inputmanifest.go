package inputmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type Entry struct {
	Path          string `json:"path"`
	Type          string `json:"type,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	Deleted       bool   `json:"deleted,omitempty"`
}

type Manifest struct {
	Entries []Entry `json:"entries"`
}

func Build(root string, inputs []string) (Manifest, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Manifest{}, fmt.Errorf("input manifest root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve input manifest root: %w", err)
	}

	seen := make(map[string]struct{})
	var paths []string
	for _, input := range inputs {
		normalized, err := normalizeInputPath(input)
		if err != nil {
			return Manifest{}, err
		}
		matches, err := expandInput(absRoot, normalized)
		if err != nil {
			return Manifest{}, err
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			paths = append(paths, match)
		}
	}
	sort.Strings(paths)

	entries := make([]Entry, 0, len(paths))
	for _, rel := range paths {
		entry, err := entryForPath(absRoot, rel)
		if err != nil {
			return Manifest{}, err
		}
		entries = append(entries, entry)
	}
	return Manifest{Entries: entries}, nil
}

func Digest(root string, inputs []string) (string, Manifest, error) {
	manifest, err := Build(root, inputs)
	if err != nil {
		return "", Manifest{}, err
	}
	payload, err := json.Marshal(manifest.Entries)
	if err != nil {
		return "", Manifest{}, fmt.Errorf("marshal input manifest: %w", err)
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), manifest, nil
}

func normalizeInputPath(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", fmt.Errorf("input path cannot be empty")
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("input path %q must be relative", input)
	}
	normalized := path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("input path %q must stay within the repository root", input)
	}
	return normalized, nil
}

func expandInput(root, input string) ([]string, error) {
	if !hasGlob(input) {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(input))); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("input path %q does not exist", input)
			}
			return nil, fmt.Errorf("stat input path %q: %w", input, err)
		}
		return []string{input}, nil
	}

	pattern := filepath.Join(root, filepath.FromSlash(input))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid input glob %q: %w", input, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("input glob %q matched no files", input)
	}
	relMatches := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(root, match)
		if err != nil {
			return nil, fmt.Errorf("resolve input glob match %q: %w", match, err)
		}
		normalized := path.Clean(filepath.ToSlash(rel))
		if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") {
			return nil, fmt.Errorf("input glob %q matched path outside root", input)
		}
		relMatches = append(relMatches, normalized)
	}
	sort.Strings(relMatches)
	return relMatches, nil
}

func hasGlob(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func entryForPath(root, rel string) (Entry, error) {
	fullPath := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, fmt.Errorf("input path %q does not exist", rel)
		}
		return Entry{}, fmt.Errorf("stat input path %q: %w", rel, err)
	}

	entry := Entry{
		Path: rel,
		Mode: uint32(info.Mode().Perm()),
	}
	mode := info.Mode()
	switch {
	case mode.Type() == 0:
		entry.Type = "file"
		digest, err := fileDigest(fullPath)
		if err != nil {
			return Entry{}, err
		}
		entry.SHA256 = digest
	case mode.IsDir():
		return Entry{}, fmt.Errorf("input path %q is a directory; inputs.files must name regular files", rel)
	case mode&os.ModeSymlink != 0:
		return Entry{}, fmt.Errorf("input path %q is a symlink; inputs.files must name regular files", rel)
	default:
		return Entry{}, fmt.Errorf("input path %q is not a regular file", rel)
	}
	return entry, nil
}

func fileDigest(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open input file %q: %w", filePath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash input file %q: %w", filePath, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

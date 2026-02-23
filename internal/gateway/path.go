package gateway

import (
	"errors"
	"net/url"
	"path"
	"strings"
)

var (
	errPathNullByte    = errors.New("path contains null byte")
	errPathTraversal   = errors.New("path contains traversal sequence")
	errPathDoubleSlash = errors.New("path contains double slash")
	errPathEncoding    = errors.New("invalid path encoding")
)

// CanonicalisePath validates and normalises an HTTP request path, rejecting
// traversal sequences, null bytes, double slashes, and percent-encoded variants.
func CanonicalisePath(raw string) (string, error) {
	if strings.ContainsRune(raw, 0) {
		return "", errPathNullByte
	}

	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", errPathEncoding
	}

	if strings.ContainsRune(decoded, 0) {
		return "", errPathNullByte
	}
	if containsTraversal(decoded) {
		return "", errPathTraversal
	}
	if strings.Contains(decoded, "//") {
		return "", errPathDoubleSlash
	}

	cleaned := path.Clean(decoded)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}

	if containsTraversal(cleaned) {
		return "", errPathTraversal
	}

	return cleaned, nil
}

func containsTraversal(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

package gateway

import (
	"testing"
)

func TestCanonicalisePathValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"/git/github.com/org/repo.git/info/refs", "/git/github.com/org/repo.git/info/refs"},
		{"/git/github.com/org/repo", "/git/github.com/org/repo"},
		{"/registry/npmjs.org/pkg", "/registry/npmjs.org/pkg"},
		{"/meta/health", "/meta/health"},
	}
	for _, tt := range tests {
		result, err := CanonicalisePath(tt.input)
		if err != nil {
			t.Errorf("CanonicalisePath(%q) error: %v", tt.input, err)
			continue
		}
		if result != tt.want {
			t.Errorf("CanonicalisePath(%q) = %q, want %q", tt.input, result, tt.want)
		}
	}
}

func TestCanonicalisePathTraversal(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/git/../secrets/key",
		"/git/../../etc/passwd",
		"/git/%2e%2e/secrets/key",
		"/git/%2E%2E/secrets/key",
		"/../../../etc/passwd",
	}
	for _, p := range paths {
		if _, err := CanonicalisePath(p); err == nil {
			t.Errorf("CanonicalisePath(%q) should have returned error", p)
		}
	}
}

func TestCanonicalisePathNullByte(t *testing.T) {
	t.Parallel()
	if _, err := CanonicalisePath("/git/github.com/\x00/repo"); err == nil {
		t.Error("expected error for null byte")
	}
	if _, err := CanonicalisePath("/git/github.com/%00/repo"); err == nil {
		t.Error("expected error for percent-encoded null byte")
	}
}

func TestCanonicalisePathDoubleSlash(t *testing.T) {
	t.Parallel()
	if _, err := CanonicalisePath("/git//github.com/repo"); err == nil {
		t.Error("expected error for double slash")
	}
}

func TestCanonicalisePathEncodedTraversal(t *testing.T) {
	t.Parallel()
	encoded := []string{
		"/git/%2e%2e/%2e%2e/secrets/",
		"/git/github.com/%2e%2e/secrets",
	}
	for _, p := range encoded {
		if _, err := CanonicalisePath(p); err == nil {
			t.Errorf("CanonicalisePath(%q) should have returned error for encoded traversal", p)
		}
	}
}

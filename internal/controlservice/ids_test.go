package controlservice

import (
	"errors"
	"strings"
	"testing"

	"go.jetify.com/typeid"
)

func TestNewIDUsesTypeIDWhenGeneratorSucceeds(t *testing.T) {
	originalGenerator := generateTypeID
	t.Cleanup(func() {
		generateTypeID = originalGenerator
	})

	generateTypeID = func(prefix string) (string, error) {
		id, err := typeid.WithPrefix(prefix)
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}

	id := newID("cr")
	parsed, err := typeid.FromString(id)
	if err != nil {
		t.Fatalf("expected generated id to be parseable typeid, got %q: %v", id, err)
	}
	if got, want := parsed.Prefix(), "cr"; got != want {
		t.Fatalf("unexpected generated id prefix: got %q want %q", got, want)
	}
}

func TestNewSandboxIDUsesDNSLabelSafeTypeIDSuffix(t *testing.T) {
	originalGenerator := generateTypeID
	t.Cleanup(func() {
		generateTypeID = originalGenerator
	})

	generateTypeID = func(prefix string) (string, error) {
		id, err := typeid.WithPrefix(prefix)
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}

	id := newSandboxID()
	parsed, err := typeid.FromString(id)
	if err != nil {
		t.Fatalf("expected generated sandbox id to be parseable typeid suffix, got %q: %v", id, err)
	}
	if got := parsed.Prefix(); got != "" {
		t.Fatalf("expected sandbox id to omit typeid prefix, got %q", got)
	}
	if strings.Contains(id, "_") {
		t.Fatalf("expected sandbox id to be DNS label safe, got %q", id)
	}
}

func TestNewSandboxIDFallbackIsDNSLabelSafe(t *testing.T) {
	originalGenerator := generateTypeID
	t.Cleanup(func() {
		generateTypeID = originalGenerator
	})

	generateTypeID = func(string) (string, error) {
		return "", errors.New("boom")
	}

	id := newSandboxID()
	if id == "" {
		t.Fatal("expected fallback sandbox id")
	}
	if !strings.HasPrefix(id, "cr-") {
		t.Fatalf("expected fallback sandbox id to keep recognizable sandbox prefix, got %q", id)
	}
	if strings.HasPrefix(id, "-") || strings.Contains(id, "_") {
		t.Fatalf("expected fallback sandbox id to be DNS label safe, got %q", id)
	}
}

func TestNewIDFallsBackToTimestampShapeWhenGeneratorFails(t *testing.T) {
	originalGenerator := generateTypeID
	t.Cleanup(func() {
		generateTypeID = originalGenerator
	})

	generateTypeID = func(string) (string, error) {
		return "", errors.New("boom")
	}

	id := newID("exec")
	if !strings.HasPrefix(id, "exec-") {
		t.Fatalf("expected fallback id to keep legacy shape, got %q", id)
	}
}

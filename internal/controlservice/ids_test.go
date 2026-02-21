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

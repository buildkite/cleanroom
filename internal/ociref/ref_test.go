package ociref

import "testing"

func TestParseDigestReferenceValid(t *testing.T) {
	t.Parallel()

	ref := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	parsed, err := ParseDigestReference(ref)
	if err != nil {
		t.Fatalf("ParseDigestReference returned error: %v", err)
	}

	if parsed.Original != ref {
		t.Fatalf("unexpected original ref: got %q want %q", parsed.Original, ref)
	}
	if parsed.Repository != "ghcr.io/buildkite/cleanroom-base/alpine" {
		t.Fatalf("unexpected repository: %q", parsed.Repository)
	}
	if parsed.Digest() != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected digest: %q", parsed.Digest())
	}
}

func TestParseDigestReferenceRejectsTagOnly(t *testing.T) {
	t.Parallel()

	if _, err := ParseDigestReference("ghcr.io/buildkite/cleanroom-base/alpine:latest"); err == nil {
		t.Fatal("expected tag-only reference to be rejected")
	}
}

func TestParseDigestReferenceRejectsBadDigest(t *testing.T) {
	t.Parallel()

	if _, err := ParseDigestReference("ghcr.io/buildkite/cleanroom-base/alpine@sha256:not-a-digest"); err == nil {
		t.Fatal("expected invalid digest to be rejected")
	}
}

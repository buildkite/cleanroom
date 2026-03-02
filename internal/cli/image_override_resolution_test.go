package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func withImageOverrideResolversForTest(
	t *testing.T,
	localFn func(context.Context, string) (string, error),
	remoteFn func(context.Context, string) (string, error),
) {
	t.Helper()
	prevLocal := importLocalDockerImageForOverrideFn
	prevRemote := resolveReferenceForPolicyUpdate
	importLocalDockerImageForOverrideFn = localFn
	resolveReferenceForPolicyUpdate = remoteFn
	t.Cleanup(func() {
		importLocalDockerImageForOverrideFn = prevLocal
		resolveReferenceForPolicyUpdate = prevRemote
	})
}

func TestResolveReferenceForImageOverridePrefersLocal(t *testing.T) {
	localCalls := 0
	remoteCalls := 0
	localRef := "local/docker-image@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	remoteRef := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	withImageOverrideResolversForTest(
		t,
		func(_ context.Context, source string) (string, error) {
			localCalls++
			if got, want := source, "alpine:latest"; got != want {
				t.Fatalf("unexpected local resolver source: got %q want %q", got, want)
			}
			return localRef, nil
		},
		func(_ context.Context, _ string) (string, error) {
			remoteCalls++
			return remoteRef, nil
		},
	)

	got, err := resolveReferenceForImageOverride(context.Background(), "alpine:latest", true)
	if err != nil {
		t.Fatalf("resolveReferenceForImageOverride returned error: %v", err)
	}
	if got != localRef {
		t.Fatalf("unexpected resolved ref: got %q want %q", got, localRef)
	}
	if localCalls != 1 {
		t.Fatalf("expected local resolver call count 1, got %d", localCalls)
	}
	if remoteCalls != 0 {
		t.Fatalf("expected remote resolver call count 0, got %d", remoteCalls)
	}
}

func TestResolveReferenceForImageOverrideFallsBackToRemote(t *testing.T) {
	localErr := errors.New("local image not found")
	remoteRef := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	withImageOverrideResolversForTest(
		t,
		func(_ context.Context, _ string) (string, error) {
			return "", localErr
		},
		func(_ context.Context, source string) (string, error) {
			if got, want := source, "alpine:latest"; got != want {
				t.Fatalf("unexpected remote resolver source: got %q want %q", got, want)
			}
			return remoteRef, nil
		},
	)

	got, err := resolveReferenceForImageOverride(context.Background(), "alpine:latest", true)
	if err != nil {
		t.Fatalf("resolveReferenceForImageOverride returned error: %v", err)
	}
	if got != remoteRef {
		t.Fatalf("unexpected resolved ref: got %q want %q", got, remoteRef)
	}
}

func TestResolveReferenceForImageOverrideReturnsCombinedError(t *testing.T) {
	withImageOverrideResolversForTest(
		t,
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("local missing")
		},
		func(_ context.Context, _ string) (string, error) {
			return "", errors.New("remote unavailable")
		},
	)

	_, err := resolveReferenceForImageOverride(context.Background(), "alpine:latest", true)
	if err == nil {
		t.Fatal("expected resolveReferenceForImageOverride to fail when local and remote resolution fail")
	}
	if !strings.Contains(err.Error(), "remote unavailable") {
		t.Fatalf("expected remote error in returned message, got %v", err)
	}
	if !strings.Contains(err.Error(), "local docker resolution failed: local missing") {
		t.Fatalf("expected local error in returned message, got %v", err)
	}
}

func TestResolveReferenceForImageOverrideSkipsLocalForRemoteEndpoint(t *testing.T) {
	localCalls := 0
	remoteCalls := 0
	remoteRef := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	withImageOverrideResolversForTest(
		t,
		func(_ context.Context, _ string) (string, error) {
			localCalls++
			return "local/docker-image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
		},
		func(_ context.Context, _ string) (string, error) {
			remoteCalls++
			return remoteRef, nil
		},
	)

	got, err := resolveReferenceForImageOverride(context.Background(), "alpine:latest", false)
	if err != nil {
		t.Fatalf("resolveReferenceForImageOverride returned error: %v", err)
	}
	if got != remoteRef {
		t.Fatalf("unexpected resolved ref: got %q want %q", got, remoteRef)
	}
	if localCalls != 0 {
		t.Fatalf("expected local resolver call count 0, got %d", localCalls)
	}
	if remoteCalls != 1 {
		t.Fatalf("expected remote resolver call count 1, got %d", remoteCalls)
	}
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func withPolicyResolveDeps(
	t *testing.T,
	digestFn func(context.Context, name.Reference) (string, error),
	platformFn func(context.Context, name.Reference) (string, string, error),
) {
	t.Helper()
	prevDigestFn := resolveRegistryDigestForReference
	prevPlatformFn := resolveReferencePlatformConfig
	resolveRegistryDigestForReference = digestFn
	resolveReferencePlatformConfig = platformFn
	t.Cleanup(func() {
		resolveRegistryDigestForReference = prevDigestFn
		resolveReferencePlatformConfig = prevPlatformFn
	})
}

func TestResolveReferenceForPolicyUpdatePreservesRegistryDigest(t *testing.T) {
	const manifestListDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platformResolverCalled := false
	withPolicyResolveDeps(
		t,
		func(_ context.Context, ref name.Reference) (string, error) {
			if got, want := ref.Context().Name(), "ghcr.io/buildkite/cleanroom-base/alpine"; got != want {
				t.Fatalf("unexpected image context: got %q want %q", got, want)
			}
			return manifestListDigest, nil
		},
		func(_ context.Context, _ name.Reference) (string, string, error) {
			platformResolverCalled = true
			return "linux", runtime.GOARCH, nil
		},
	)

	got, err := resolveReferenceForPolicyUpdate(context.Background(), "ghcr.io/buildkite/cleanroom-base/alpine:latest")
	if err != nil {
		t.Fatalf("resolveReferenceForPolicyUpdate returned error: %v", err)
	}
	if want := "ghcr.io/buildkite/cleanroom-base/alpine@" + manifestListDigest; got != want {
		t.Fatalf("expected resolver to preserve registry digest: got %q want %q", got, want)
	}
	if platformResolverCalled {
		t.Fatal("resolveReferenceForPolicyUpdate should not validate platform")
	}
}

func TestResolveReferenceForImageOverrideLocalValidatesResolvedDigestPlatform(t *testing.T) {
	incompatibleArch := "amd64"
	if runtime.GOARCH == "amd64" {
		incompatibleArch = "arm64"
	}
	prevLocal := importLocalDockerImageForOverrideFn
	prevResolve := resolveReferenceForPolicyUpdate
	prevPlatform := resolveReferencePlatformConfig
	importLocalDockerImageForOverrideFn = func(context.Context, string) (string, error) {
		return "", errors.New("local missing")
	}
	resolveReferenceForPolicyUpdate = func(context.Context, string) (string, error) {
		return "ghcr.io/buildkite/cleanroom-base/alpine@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nil
	}
	resolveReferencePlatformConfig = func(_ context.Context, ref name.Reference) (string, string, error) {
		if got, want := ref.String(), "ghcr.io/buildkite/cleanroom-base/alpine@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"; got != want {
			return "", "", fmt.Errorf("unexpected platform validation ref: got %q want %q", got, want)
		}
		return "linux", incompatibleArch, nil
	}
	t.Cleanup(func() {
		importLocalDockerImageForOverrideFn = prevLocal
		resolveReferenceForPolicyUpdate = prevResolve
		resolveReferencePlatformConfig = prevPlatform
	})

	_, err := resolveReferenceForImageOverride(context.Background(), "ghcr.io/buildkite/cleanroom-base/alpine:latest", true)
	if err == nil {
		t.Fatal("expected incompatible platform error")
	}
	if got, want := strings.ToLower(err.Error()), "incompatible"; !strings.Contains(got, want) {
		t.Fatalf("expected incompatibility error, got %v", err)
	}
}

func TestResolveReferenceForImageOverrideRemoteSkipsPlatformValidation(t *testing.T) {
	prevResolve := resolveReferenceForPolicyUpdate
	prevPlatform := resolveReferencePlatformConfig
	resolveReferenceForPolicyUpdate = func(context.Context, string) (string, error) {
		return "ghcr.io/buildkite/cleanroom-base/alpine@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", nil
	}
	resolveReferencePlatformConfig = func(context.Context, name.Reference) (string, string, error) {
		return "", "", errors.New("platform resolver should not be called for remote endpoints")
	}
	t.Cleanup(func() {
		resolveReferenceForPolicyUpdate = prevResolve
		resolveReferencePlatformConfig = prevPlatform
	})

	got, err := resolveReferenceForImageOverride(context.Background(), "ghcr.io/buildkite/cleanroom-base/alpine:latest", false)
	if err != nil {
		t.Fatalf("resolveReferenceForImageOverride returned error: %v", err)
	}
	if want := "ghcr.io/buildkite/cleanroom-base/alpine@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"; got != want {
		t.Fatalf("unexpected resolved ref: got %q want %q", got, want)
	}
}

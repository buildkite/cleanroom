package cli

import (
	"context"
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
	withPolicyResolveDeps(
		t,
		func(_ context.Context, ref name.Reference) (string, error) {
			if got, want := ref.Context().Name(), "ghcr.io/buildkite/cleanroom-base/alpine"; got != want {
				t.Fatalf("unexpected image context: got %q want %q", got, want)
			}
			return manifestListDigest, nil
		},
		func(_ context.Context, _ name.Reference) (string, string, error) {
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
}

func TestResolveReferenceForPolicyUpdateRejectsIncompatiblePlatform(t *testing.T) {
	incompatibleArch := "amd64"
	if runtime.GOARCH == "amd64" {
		incompatibleArch = "arm64"
	}
	withPolicyResolveDeps(
		t,
		func(_ context.Context, _ name.Reference) (string, error) {
			return "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
		},
		func(_ context.Context, _ name.Reference) (string, string, error) {
			return "linux", incompatibleArch, nil
		},
	)

	_, err := resolveReferenceForPolicyUpdate(context.Background(), "ghcr.io/buildkite/cleanroom-base/alpine:latest")
	if err == nil {
		t.Fatal("expected incompatible platform error")
	}
	if got, want := strings.ToLower(err.Error()), "incompatible"; !strings.Contains(got, want) {
		t.Fatalf("expected incompatibility error, got %v", err)
	}
}

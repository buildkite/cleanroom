package darwinvz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/policy"
)

func TestResolveRootFSPathUsesConfiguredRootFS(t *testing.T) {
	t.Parallel()

	configuredPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(configuredPath, []byte("fake-ext4"), 0o644); err != nil {
		t.Fatalf("write configured rootfs: %v", err)
	}

	adapter := New()
	req := backend.RunRequest{
		Policy: &policy.CompiledPolicy{
			ImageRef:    "ghcr.io/buildkite/cleanroom-base/alpine@sha256:abc",
			ImageDigest: "sha256:abc",
		},
		FirecrackerConfig: backend.FirecrackerConfig{
			RootFSPath: configuredPath,
		},
	}

	path, ref, digest, notice, err := adapter.resolveRootFSPath(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveRootFSPath returned error: %v", err)
	}
	if got, want := path, configuredPath; got != want {
		t.Fatalf("unexpected rootfs path: got %q want %q", got, want)
	}
	if got, want := ref, req.Policy.ImageRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
	if got, want := digest, req.Policy.ImageDigest; got != want {
		t.Fatalf("unexpected image digest: got %q want %q", got, want)
	}
	if notice != "" {
		t.Fatalf("expected empty notice, got %q", notice)
	}
}

func TestResolveRootFSPathDerivesFromPolicyImageRef(t *testing.T) {
	t.Parallel()

	adapter := New()
	adapter.ensurePreparedRootFSFn = func(_ context.Context, imageRef string) (preparedRootFS, error) {
		if got, want := imageRef, "ghcr.io/buildkite/cleanroom-base/alpine@sha256:def"; got != want {
			t.Fatalf("unexpected image ref: got %q want %q", got, want)
		}
		return preparedRootFS{
			Ref:    imageRef,
			Digest: "sha256:def",
			Path:   "/tmp/prepared.ext4",
			Hit:    true,
		}, nil
	}

	req := backend.RunRequest{
		Policy: &policy.CompiledPolicy{
			ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:def",
		},
	}

	path, ref, digest, notice, err := adapter.resolveRootFSPath(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveRootFSPath returned error: %v", err)
	}
	if got, want := path, "/tmp/prepared.ext4"; got != want {
		t.Fatalf("unexpected rootfs path: got %q want %q", got, want)
	}
	if got, want := ref, req.Policy.ImageRef; got != want {
		t.Fatalf("unexpected image ref: got %q want %q", got, want)
	}
	if got, want := digest, "sha256:def"; got != want {
		t.Fatalf("unexpected image digest: got %q want %q", got, want)
	}
	if notice == "" {
		t.Fatal("expected non-empty derivation notice")
	}
}

func TestResolveRootFSPathRequiresImageRefWhenRootFSUnset(t *testing.T) {
	t.Parallel()

	adapter := New()
	req := backend.RunRequest{
		Policy: &policy.CompiledPolicy{},
	}
	_, _, _, _, err := adapter.resolveRootFSPath(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when both rootfs and image ref are unset")
	}
}

func TestResolveRootFSPathFallsBackWhenConfiguredRootFSMissing(t *testing.T) {
	t.Parallel()

	adapter := New()
	adapter.ensurePreparedRootFSFn = func(_ context.Context, imageRef string) (preparedRootFS, error) {
		return preparedRootFS{
			Ref:    imageRef,
			Digest: "sha256:xyz",
			Path:   "/tmp/prepared-missing-fallback.ext4",
			Hit:    false,
		}, nil
	}

	req := backend.RunRequest{
		Policy: &policy.CompiledPolicy{
			ImageRef: "ghcr.io/buildkite/cleanroom-base/alpine@sha256:xyz",
		},
		FirecrackerConfig: backend.FirecrackerConfig{
			RootFSPath: "/tmp/definitely-missing-cleanroom-rootfs.ext4",
		},
	}

	path, _, digest, notice, err := adapter.resolveRootFSPath(context.Background(), req)
	if err != nil {
		t.Fatalf("resolveRootFSPath returned error: %v", err)
	}
	if got, want := path, "/tmp/prepared-missing-fallback.ext4"; got != want {
		t.Fatalf("unexpected rootfs path: got %q want %q", got, want)
	}
	if got, want := digest, "sha256:xyz"; got != want {
		t.Fatalf("unexpected digest: got %q want %q", got, want)
	}
	if notice == "" {
		t.Fatal("expected fallback notice")
	}
}

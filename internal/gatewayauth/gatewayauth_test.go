package gatewayauth

import "testing"

func TestGitRepoPrefixFromURLNormalizesRepositorySuffix(t *testing.T) {
	t.Parallel()

	got, err := GitRepoPrefixFromURL("https://github.com/buildkite/cleanroom.git")
	if err != nil {
		t.Fatalf("GitRepoPrefixFromURL returned error: %v", err)
	}
	if want := "github.com/buildkite/cleanroom"; got != want {
		t.Fatalf("unexpected prefix: got %q want %q", got, want)
	}
}

func TestGitRepoKeyFromRequestMatchesRemoteWithoutGitSuffix(t *testing.T) {
	t.Parallel()

	got, err := GitRepoKeyFromRequest("github.com", "/buildkite/cleanroom.git/info/refs")
	if err != nil {
		t.Fatalf("GitRepoKeyFromRequest returned error: %v", err)
	}
	if !AllowsGitRepo([]string{"github.com/buildkite/cleanroom"}, got) {
		t.Fatalf("expected repo key %q to be allowed", got)
	}
}

func TestOCIRepoPrefixFromImageRefDefaultsDockerHubLibrary(t *testing.T) {
	t.Parallel()

	got, err := OCIRepoPrefixFromImageRef("alpine@sha256:0123456789abcdef")
	if err != nil {
		t.Fatalf("OCIRepoPrefixFromImageRef returned error: %v", err)
	}
	if want := "docker.io/library/alpine"; got != want {
		t.Fatalf("unexpected prefix: got %q want %q", got, want)
	}
}

func TestOCIRepoKeyFromPathExtractsRepository(t *testing.T) {
	t.Parallel()

	got, ok, err := OCIRepoKeyFromPath("ghcr.io", "buildkite/cleanroom/manifests/sha256:abc")
	if err != nil {
		t.Fatalf("OCIRepoKeyFromPath returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected repository to be present")
	}
	if want := "ghcr.io/buildkite/cleanroom"; got != want {
		t.Fatalf("unexpected key: got %q want %q", got, want)
	}
}

func TestOCIRepoKeyFromPathUsesDistributionMarkerFromRight(t *testing.T) {
	t.Parallel()

	got, ok, err := OCIRepoKeyFromPath("ghcr.io", "org/tags/manifests/image/manifests/latest")
	if err != nil {
		t.Fatalf("OCIRepoKeyFromPath returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected repository to be present")
	}
	if want := "ghcr.io/org/tags/manifests/image"; got != want {
		t.Fatalf("unexpected key: got %q want %q", got, want)
	}
}

func TestOCIRepoKeyFromPathAllowsVersionProbeWithoutRepository(t *testing.T) {
	t.Parallel()

	got, ok, err := OCIRepoKeyFromPath("docker.io", "v2/")
	if err != nil {
		t.Fatalf("OCIRepoKeyFromPath returned error: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("expected no repository for version probe, got ok=%v key=%q", ok, got)
	}
}

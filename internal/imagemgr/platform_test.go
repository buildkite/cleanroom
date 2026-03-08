package imagemgr

import (
	"strings"
	"testing"
)

func TestValidateImagePlatformForHostAcceptsMatchingLinuxArch(t *testing.T) {
	t.Parallel()

	if err := ValidateImagePlatformForHost("linux", "arm64", "arm64"); err != nil {
		t.Fatalf("ValidateImagePlatformForHost returned error for matching platform: %v", err)
	}
}

func TestValidateImagePlatformForHostNormalizesEquivalentArchitectures(t *testing.T) {
	t.Parallel()

	if err := ValidateImagePlatformForHost("linux", "x86_64", "amd64"); err != nil {
		t.Fatalf("ValidateImagePlatformForHost should treat x86_64 and amd64 as equivalent: %v", err)
	}
}

func TestValidateImagePlatformForHostRejectsMismatchedArchitecture(t *testing.T) {
	t.Parallel()

	err := ValidateImagePlatformForHost("linux", "amd64", "arm64")
	if err == nil {
		t.Fatal("expected architecture mismatch to be rejected")
	}
	if got, want := err.Error(), "linux/amd64"; !strings.Contains(got, want) {
		t.Fatalf("expected mismatch error to mention image architecture %q, got %q", want, got)
	}
	if got, want := err.Error(), "linux/arm64"; !strings.Contains(got, want) {
		t.Fatalf("expected mismatch error to mention host architecture %q, got %q", want, got)
	}
}

func TestValidateImagePlatformForHostRejectsNonLinuxImageOS(t *testing.T) {
	t.Parallel()

	err := ValidateImagePlatformForHost("windows", "amd64", "amd64")
	if err == nil {
		t.Fatal("expected non-linux image OS to be rejected")
	}
	if got, want := err.Error(), "unsupported"; !strings.Contains(got, want) {
		t.Fatalf("expected unsupported OS error, got %q", got)
	}
}

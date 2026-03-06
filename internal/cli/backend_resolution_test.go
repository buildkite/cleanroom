package cli

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestResolveBackendNameDefaultsToHostBackend(t *testing.T) {
	if got, want := resolveBackendName("", ""), runtimeconfig.DefaultBackendForHost(); got != want {
		t.Fatalf("resolveBackendName fallback = %q, want %q", got, want)
	}
}

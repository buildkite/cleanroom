package cli

import (
	"testing"

	"github.com/buildkite/cleanroom/internal/runtimeconfig"
)

func TestClientFlagsResolvedHostPrefersExplicitHost(t *testing.T) {
	t.Parallel()

	flags := clientFlags{Host: "unix:///tmp/explicit.sock"}
	cfg := runtimeconfig.Config{ControlHost: "unix:///tmp/from-config.sock"}

	if got, want := flags.resolvedHost(cfg), "unix:///tmp/explicit.sock"; got != want {
		t.Fatalf("resolvedHost mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolvedHostUsesConfigFallback(t *testing.T) {
	t.Parallel()

	flags := clientFlags{}
	cfg := runtimeconfig.Config{ControlHost: "unix:///tmp/from-config.sock"}

	if got, want := flags.resolvedHost(cfg), "unix:///tmp/from-config.sock"; got != want {
		t.Fatalf("resolvedHost mismatch: got %q want %q", got, want)
	}
}

func TestClientFlagsResolvedHostReturnsEmptyWhenUnset(t *testing.T) {
	t.Parallel()

	flags := clientFlags{}
	cfg := runtimeconfig.Config{}

	if got := flags.resolvedHost(cfg); got != "" {
		t.Fatalf("resolvedHost mismatch: got %q want empty", got)
	}
}

package cli

import (
	"context"
	"testing"

	"github.com/buildkite/cleanroom/internal/backend"
	backendfirecracker "github.com/buildkite/cleanroom/internal/backend/firecracker"
)

func stubFirecrackerHostSupport(t *testing.T, fn func(context.Context, backend.FirecrackerConfig) backendfirecracker.HostSupport) {
	t.Helper()
	prev := detectFirecrackerHostSupport
	detectFirecrackerHostSupport = fn
	t.Cleanup(func() {
		detectFirecrackerHostSupport = prev
	})
}

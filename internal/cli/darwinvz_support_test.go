package cli

import (
	"testing"

	backenddarwinvz "github.com/buildkite/cleanroom/internal/backend/darwinvz"
)

func stubDarwinVZSnapshotSupport(t *testing.T, fn func() backenddarwinvz.SnapshotSupport) {
	t.Helper()
	prev := detectDarwinVZSnapshotSupport
	detectDarwinVZSnapshotSupport = fn
	t.Cleanup(func() {
		detectDarwinVZSnapshotSupport = prev
	})
}

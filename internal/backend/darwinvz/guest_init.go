//go:build darwin

package darwinvz

import (
	"fmt"

	"github.com/buildkite/cleanroom/internal/hosttools"
)

const (
	guestAgentPath          = "/usr/local/bin/cleanroom-guest-agent"
	guestRuntimeInitVersion = "go-init-v1"
)

func guestInitExecutableForRootFS(_ string) (path, notice string) {
	return guestAgentPath, ""
}

func (a *Adapter) installGuestRuntimeIntoRootFS(rootFSPath, guestAgentBinaryPath string) error {
	if _, err := hosttools.ResolveE2FSProgsBinary("debugfs"); err != nil {
		return fmt.Errorf("find debugfs for runtime rootfs preparation: %w", err)
	}

	if err := injectFileIntoExt4(rootFSPath, guestAgentBinaryPath, guestAgentPath, 0o755); err != nil {
		return fmt.Errorf("inject guest agent into rootfs image: %w", err)
	}
	return nil
}

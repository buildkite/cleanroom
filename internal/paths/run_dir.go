package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// RunBaseDir resolves the default base directory for run artifacts.
// Preference order:
// 1. $XDG_RUNTIME_DIR/cleanroom/runs
// 2. $XDG_STATE_HOME/cleanroom/runs
// 3. ~/.local/state/cleanroom/runs
func RunBaseDir() (string, error) {
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "cleanroom", "runs"), nil
	}
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
		return filepath.Join(stateHome, "cleanroom", "runs"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "cleanroom", "runs"), nil
}

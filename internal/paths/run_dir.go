package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// RunBaseDir resolves the default base directory for run artifacts.
// Preference order:
// 1. $XDG_STATE_HOME/cleanroom/runs
// 2. ~/.local/state/cleanroom/runs
// 3. $XDG_RUNTIME_DIR/cleanroom/runs
func RunBaseDir() (string, error) {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
		return filepath.Join(stateHome, "cleanroom", "runs"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
			return filepath.Join(runtimeDir, "cleanroom", "runs"), nil
		}
		return "", err
	}
	if home != "" {
		return filepath.Join(home, ".local", "state", "cleanroom", "runs"), nil
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "cleanroom", "runs"), nil
	}
	return "", errors.New("unable to resolve run directory from XDG state/runtime or home")
}

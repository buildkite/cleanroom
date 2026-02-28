package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DataBaseDir resolves the default base directory for cleanroom durable data.
// Preference order:
// 1. $XDG_DATA_HOME/cleanroom
// 2. ~/.local/share/cleanroom
// 3. $XDG_RUNTIME_DIR/cleanroom
func DataBaseDir() (string, error) {
	if dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dataHome != "" {
		return filepath.Join(dataHome, "cleanroom"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
			return filepath.Join(runtimeDir, "cleanroom"), nil
		}
		return "", err
	}
	if home != "" {
		return filepath.Join(home, ".local", "share", "cleanroom"), nil
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "cleanroom"), nil
	}
	return "", errors.New("unable to resolve data directory from XDG data/runtime or home")
}

func AssetsDir() (string, error) {
	base, err := DataBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "assets"), nil
}

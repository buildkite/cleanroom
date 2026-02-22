package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// StateBaseDir resolves the default base directory for cleanroom state.
// Preference order:
// 1. $XDG_STATE_HOME/cleanroom
// 2. ~/.local/state/cleanroom
// 3. $XDG_RUNTIME_DIR/cleanroom
func StateBaseDir() (string, error) {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
		return filepath.Join(stateHome, "cleanroom"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
			return filepath.Join(runtimeDir, "cleanroom"), nil
		}
		return "", err
	}
	if home != "" {
		return filepath.Join(home, ".local", "state", "cleanroom"), nil
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "cleanroom"), nil
	}
	return "", errors.New("unable to resolve state directory from XDG state/runtime or home")
}

func TSNetStateDir() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tsnet"), nil
}

// TLSDir returns the default directory for cleanroom TLS material.
// Uses $XDG_CONFIG_HOME/cleanroom/tls or ~/.config/cleanroom/tls.
func TLSDir() (string, error) {
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome != "" {
		return filepath.Join(configHome, "cleanroom", "tls"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cleanroom", "tls"), nil
}

package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// CacheBaseDir resolves the default base directory for cleanroom cache.
// Preference order:
// 1. $XDG_CACHE_HOME/cleanroom
// 2. ~/.cache/cleanroom
// 3. $XDG_RUNTIME_DIR/cleanroom
func CacheBaseDir() (string, error) {
	if cacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cacheHome != "" {
		return filepath.Join(cacheHome, "cleanroom"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
			return filepath.Join(runtimeDir, "cleanroom"), nil
		}
		return "", err
	}
	if home != "" {
		return filepath.Join(home, ".cache", "cleanroom"), nil
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		return filepath.Join(runtimeDir, "cleanroom"), nil
	}
	return "", errors.New("unable to resolve cache directory from XDG cache/runtime or home")
}

func ImageCacheDir() (string, error) {
	base, err := CacheBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "images"), nil
}

func ImageMetadataDBPath() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "images", "metadata.db"), nil
}

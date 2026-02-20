package paths

import (
	"path/filepath"
)

// RunBaseDir resolves the default base directory for run artifacts.
// Preference order:
// 1. $XDG_STATE_HOME/cleanroom/runs
// 2. ~/.local/state/cleanroom/runs
// 3. $XDG_RUNTIME_DIR/cleanroom/runs
func RunBaseDir() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "runs"), nil
}

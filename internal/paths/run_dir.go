package paths

import (
	"path/filepath"
)

// ExecutionBaseDir resolves the default base directory for execution artifacts.
// Preference order:
// 1. $XDG_STATE_HOME/cleanroom/executions
// 2. ~/.local/state/cleanroom/executions
// 3. $XDG_RUNTIME_DIR/cleanroom/executions
func ExecutionBaseDir() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "executions"), nil
}

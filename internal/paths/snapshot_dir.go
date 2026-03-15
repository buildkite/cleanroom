package paths

import "path/filepath"

func SnapshotDir() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "snapshots"), nil
}

func SnapshotMetadataDBPath() (string, error) {
	base, err := StateBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "snapshots", "metadata.db"), nil
}

//go:build !darwin

package darwinvz

import "fmt"

type SnapshotSupport struct {
	Usable  bool
	Message string
}

func DetectSnapshotSupport() SnapshotSupport {
	return SnapshotSupport{Message: fmt.Sprintf("darwin-vz snapshots require macOS")}
}

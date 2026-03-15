//go:build !darwin

package volumestore

import "errors"

type APFSDriverOptions struct {
	SnapshotBaseDir string
	Namespace       string
}

type APFSDriver struct{}

func NewAPFSDriver(APFSDriverOptions) (*APFSDriver, error) {
	return nil, errors.New("apfs volume driver is darwin-only")
}

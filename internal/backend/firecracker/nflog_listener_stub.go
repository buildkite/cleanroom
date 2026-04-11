//go:build !linux

package firecracker

import (
	"net/netip"

	"github.com/buildkite/cleanroom/internal/dnsproxy"
)

type nflogListenerConfig struct {
	group     uint16
	sandboxID string
	guestIP   netip.Addr
	runtime   *dnsproxy.Runtime
	onBlocked func(string)
}

type nflogListener struct{}

func nflogGroupFromTapName(_ string) uint16 {
	return 0
}

func newNFLogListener(_ nflogListenerConfig) (*nflogListener, error) {
	return nil, nil
}

func (l *nflogListener) Close() error {
	return nil
}

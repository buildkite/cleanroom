//go:build linux

package vsockhttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/mdlayher/vsock"
)

const DefaultGatewayPort uint32 = 17080

var dialVsock = func(contextID, port uint32, cfg *vsock.Config) (net.Conn, error) {
	return vsock.Dial(contextID, port, cfg)
}

// NewHostTransport returns an HTTP transport that always dials the hypervisor
// via AF_VSOCK on the given port.
func NewHostTransport(port uint32) *http.Transport {
	if port == 0 {
		port = DefaultGatewayPort
	}

	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			conn, err := dialVsock(vsock.Hypervisor, port, nil)
			if err != nil {
				return nil, fmt.Errorf("dial host vsock port %d: %w", port, err)
			}
			return conn, nil
		},
	}
}

func NewHostClient(port uint32, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Transport: NewHostTransport(port),
		Timeout:   timeout,
	}
}

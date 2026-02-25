package firecracker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/buildkite/cleanroom/internal/backend"
)

// SetupGatewayFirewall installs global iptables rules that restrict access to
// the gateway port to loopback and TAP interfaces (cr+) only. All other
// interfaces (e.g. eth0) are blocked from reaching the gateway.
//
// The rules are structured so that TAP traffic is NOT accepted here — it falls
// through to the per-TAP anti-spoof and accept rules installed by
// setupHostNetwork. This preserves sandbox identity isolation.
//
// Returns a cleanup function that removes the rules. The caller must invoke
// cleanup on shutdown.
func SetupGatewayFirewall(ctx context.Context, port int, cfg backend.FirecrackerConfig) (cleanup func(), err error) {
	run := func(ctx context.Context, args ...string) error {
		return runRootCommand(ctx, cfg, args...)
	}
	return setupGatewayFirewall(ctx, port, run)
}

func setupGatewayFirewall(ctx context.Context, port int, run rootCommandFunc) (cleanup func(), err error) {
	portStr := strconv.Itoa(port)

	// Allow loopback access to gateway port.
	if err := run(ctx, "iptables", "-A", "INPUT", "-i", "lo", "-p", "tcp", "--dport", portStr, "-j", "ACCEPT"); err != nil {
		return nil, fmt.Errorf("install gateway loopback rule: %w", err)
	}

	// Drop gateway traffic from non-TAP interfaces (eth0, docker0, etc.).
	// TAP traffic (cr*) is intentionally NOT matched here so it falls through
	// to the per-TAP anti-spoof rules installed by setupHostNetwork.
	if err := run(ctx, "iptables", "-A", "INPUT", "!", "-i", "cr+", "-p", "tcp", "--dport", portStr, "-j", "DROP"); err != nil {
		_ = run(ctx, "iptables", "-D", "INPUT", "-i", "lo", "-p", "tcp", "--dport", portStr, "-j", "ACCEPT")
		return nil, fmt.Errorf("install gateway drop rule: %w", err)
	}

	cleanup = func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = run(cleanupCtx, "iptables", "-D", "INPUT", "!", "-i", "cr+", "-p", "tcp", "--dport", portStr, "-j", "DROP")
		_ = run(cleanupCtx, "iptables", "-D", "INPUT", "-i", "lo", "-p", "tcp", "--dport", portStr, "-j", "ACCEPT")
	}
	return cleanup, nil
}

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/dnsproxy"
	"github.com/buildkite/cleanroom/internal/policy"
)

type hostNetworkConfig struct {
	TapName         string
	HostIP          string
	GuestIP         string
	GuestDNS        string
	PolicyResolveMS int64
}

type ipLookupFunc func(ctx context.Context, host string) ([]net.IP, error)
type interfaceLookupFunc func(name string) (*net.Interface, error)
type rootCommandBatchFunc func(ctx context.Context, commands [][]string) error

func setupHostNetwork(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, runner privilegedCommandRunner, onDeny func(sandboxID, queryName string), onBlocked func(string)) (hostNetworkConfig, func(), error) {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	return setupHostNetworkWithDeps(ctx, runID, allowAll, allow, gatewayPort, lookup, runner, onDeny, onBlocked)
}

func setupHostNetworkWithDeps(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, runner privilegedCommandRunner, onDeny func(sandboxID, queryName string), onBlocked func(string)) (hostNetworkConfig, func(), error) {
	return setupHostNetworkWithTrustedDNSFactory(ctx, runID, allowAll, allow, gatewayPort, lookup, net.InterfaceByName, runner, newTrustedDNSService, onDeny, onBlocked)
}

func setupHostNetworkWithTapLookup(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, interfaceByName interfaceLookupFunc, runner privilegedCommandRunner, onDeny func(sandboxID, queryName string), onBlocked func(string)) (hostNetworkConfig, func(), error) {
	return setupHostNetworkWithTrustedDNSFactory(ctx, runID, allowAll, allow, gatewayPort, lookup, interfaceByName, runner, newTrustedDNSService, onDeny, onBlocked)
}

func setupHostNetworkWithTrustedDNSFactory(ctx context.Context, runID string, allowAll bool, allow []policy.AllowRule, gatewayPort int, lookup ipLookupFunc, interfaceByName interfaceLookupFunc, runner privilegedCommandRunner, factory trustedDNSFactory, onDeny func(sandboxID, queryName string), onBlocked func(string)) (hostNetworkConfig, func(), error) {
	_ = lookup
	if factory == nil {
		factory = newTrustedDNSService
	}

	tapName := tapNameFromExecutionID(runID)
	hostIP, guestIP := hostGuestIPs(runID)
	hostCIDR := hostIP + "/24"
	guestCIDR := guestIP + "/32"
	hostAddr, err := netip.ParseAddr(hostIP)
	if err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("parse host ip %q: %w", hostIP, err)
	}
	guestAddr, err := netip.ParseAddr(guestIP)
	if err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("parse guest ip %q: %w", guestIP, err)
	}

	setupRun := func(args ...string) error {
		return runner.Run(ctx, args...)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), networkCleanupTimeout)
	cleanupCmds := make([][]string, 0, 16)
	trustedDNSCleanup := func() {}
	nflogCleanupFn := func() {}
	cleanup := func() {
		defer cleanupCancel()
		nflogCleanupFn()
		trustedDNSCleanup()
		reversed := make([][]string, 0, len(cleanupCmds))
		for i := len(cleanupCmds) - 1; i >= 0; i-- {
			reversed = append(reversed, cleanupCmds[i])
		}
		for _, args := range reversed {
			if isTapDeleteCommand(args, tapName) {
				_ = deleteTapDeviceWithRetry(cleanupCtx, tapName, tapDeleteRetryInterval, interfaceByName, runner)
				continue
			}
			_ = runner.Run(cleanupCtx, args...)
		}
	}
	addCleanup := func(args ...string) {
		cleanupCmds = append(cleanupCmds, append([]string(nil), args...))
	}
	removeNFLogRule := func(tapName, groupStr string) {
		args := []string{"iptables", "-D", "FORWARD", "-i", tapName, "-j", "NFLOG", "--nflog-group", groupStr}
		if err := runner.Run(cleanupCtx, args...); err != nil {
			log.Printf("nflog iptables cleanup failed for %s: %v", tapName, err)
			addCleanup(args...)
		}
	}

	staleTapCleanupCtx, staleTapCleanupCancel := context.WithTimeout(context.Background(), networkCleanupTimeout)
	defer staleTapCleanupCancel()
	if _, err := interfaceByName(tapName); err == nil {
		if err := deleteTapDeviceWithRetry(staleTapCleanupCtx, tapName, tapDeleteRetryInterval, interfaceByName, runner); err != nil {
			return hostNetworkConfig{}, func() {}, fmt.Errorf("remove stale tap device %s: %w", tapName, err)
		}
	} else if !isNoSuchNetworkInterfaceError(err) {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("lookup tap device %s: %w", tapName, err)
	}

	if err := setupRun("ip", "tuntap", "add", "dev", tapName, "mode", "tap", "user", strconv.Itoa(os.Getuid())); err != nil {
		return hostNetworkConfig{}, func() {}, fmt.Errorf("create tap device %s: %w", tapName, err)
	}
	addCleanup("ip", "link", "del", tapName)
	if err := setupRun("ip", "addr", "add", hostCIDR, "dev", tapName); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("assign host ip to %s: %w", tapName, err)
	}
	if err := setupRun("ip", "link", "set", "dev", tapName, "up"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("bring tap %s up: %w", tapName, err)
	}
	if err := setupRun("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("enable ipv4 forwarding: %w", err)
	}

	// Disable IPv6 on TAP to prevent bypass of IPv4-only policy controls.
	if err := setupRun("sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", tapName)); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("disable ipv6 on %s: %w", tapName, err)
	}

	if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "!", "-s", guestIP, "-j", "DROP"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install anti-spoof rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "INPUT", "-i", tapName, "!", "-s", guestIP, "-j", "DROP")

	if gatewayPort > 0 {
		port := strconv.Itoa(gatewayPort)
		if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-s", guestIP, "-p", "tcp", "--dport", port, "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install gateway accept rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-s", guestIP, "-p", "tcp", "--dport", port, "-j", "ACCEPT")
	}

	for _, proto := range []string{"udp", "tcp"} {
		if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-s", guestIP, "-p", proto, "--dport", strconv.Itoa(trustedDNSListenPort), "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns %s accept rule for %s: %w", proto, tapName, err)
		}
		addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-s", guestIP, "-p", proto, "--dport", strconv.Itoa(trustedDNSListenPort), "-j", "ACCEPT")
	}

	if err := setupRun("iptables", "-A", "INPUT", "-i", tapName, "-j", "DROP"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install input deny rule for %s: %w", tapName, err)
	}
	addCleanup("iptables", "-D", "INPUT", "-i", tapName, "-j", "DROP")

	if err := setupRun("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE"); err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install nat rule for %s: %w", guestCIDR, err)
	}
	addCleanup("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", guestCIDR, "-j", "MASQUERADE")

	for _, proto := range []string{"udp", "tcp"} {
		if err := setupRun("iptables", "-t", "nat", "-A", "PREROUTING", "-i", tapName, "-p", proto, "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(trustedDNSListenPort)); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns %s redirect for %s: %w", proto, tapName, err)
		}
		addCleanup("iptables", "-t", "nat", "-D", "PREROUTING", "-i", tapName, "-p", proto, "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(trustedDNSListenPort))
	}

	returnPathCleanup, err := installForwardReturnPathRule(setupRun, tapName)
	if err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward return-path rule for %s: %w", tapName, err)
	}
	addCleanup(returnPathCleanup...)

	tcpChainName := ""
	udpChainName := ""
	if !allowAll {
		tcpChainName = trustedDNSTCPChainName(tapName)
		udpChainName = trustedDNSUDPChainName(tapName)

		if err := setupRun("iptables", "-N", tcpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("create trusted dns tcp chain for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-X", tcpChainName)
		addCleanup("iptables", "-F", tcpChainName)
		if err := setupRun("iptables", "-N", udpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("create trusted dns udp chain for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-X", udpChainName)
		addCleanup("iptables", "-F", udpChainName)

		establishedCleanup, err := installForwardEstablishedEgressRule(setupRun, tapName)
		if err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install forward established egress rule for %s: %w", tapName, err)
		}
		addCleanup(establishedCleanup...)

		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "tcp", "-j", tcpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns tcp chain jump for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "tcp", "-j", tcpChainName)
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-p", "udp", "-j", udpChainName); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install trusted dns udp chain jump for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-p", "udp", "-j", udpChainName)

		for _, rule := range literalIPv4AllowRules(allow) {
			for _, proto := range []string{"tcp", "udp"} {
				for _, port := range rule.Ports {
					portText := strconv.Itoa(port)
					if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-d", rule.Host, "-p", proto, "--dport", portText, "-j", "ACCEPT"); err != nil {
						cleanup()
						return hostNetworkConfig{}, func() {}, fmt.Errorf("install direct-ip %s allow rule for %s: %w", proto, rule.Host, err)
					}
					addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-d", rule.Host, "-p", proto, "--dport", portText, "-j", "ACCEPT")
				}
			}
		}
	}

	var dnsRuntime *dnsproxy.Runtime
	trustedDNSCleanup, dnsRuntime, err = factory(ctx, trustedDNSConfig{
		sandboxID:    runID,
		hostIP:       hostAddr,
		guestIP:      guestAddr,
		policy:       trustedDNSPolicy(allowAll, allow),
		runBatch:     runner.RunBatch,
		tcpChainName: tcpChainName,
		udpChainName: udpChainName,
		onDeny:       onDeny,
	})
	if err != nil {
		cleanup()
		return hostNetworkConfig{}, func() {}, fmt.Errorf("start trusted dns service: %w", err)
	}

	if allowAll {
		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "ACCEPT"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install allow-all forward rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "ACCEPT")
	} else {
		nflogGroup := nflogGroupFromTapNameFn(tapName)
		if nflogGroup > 0 && onBlocked != nil && dnsRuntime != nil {
			groupStr := strconv.Itoa(int(nflogGroup))
			if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "NFLOG", "--nflog-group", groupStr); err != nil {
				log.Printf("nflog iptables rule unavailable for %s: %v", tapName, err)
			} else {
				listener, nflogErr := newNFLogListenerFn(nflogListenerConfig{
					group:     nflogGroup,
					sandboxID: runID,
					guestIP:   guestAddr,
					runtime:   dnsRuntime,
					onBlocked: onBlocked,
				})
				if nflogErr != nil {
					log.Printf("nflog listener unavailable for %s: %v", runID, nflogErr)
					removeNFLogRule(tapName, groupStr)
				} else if listener == nil {
					removeNFLogRule(tapName, groupStr)
				} else {
					addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "NFLOG", "--nflog-group", groupStr)
					nflogCleanupFn = func() { _ = listener.Close() }
				}
			}
		}

		if err := setupRun("iptables", "-A", "FORWARD", "-i", tapName, "-j", "DROP"); err != nil {
			cleanup()
			return hostNetworkConfig{}, func() {}, fmt.Errorf("install default deny forward rule for %s: %w", tapName, err)
		}
		addCleanup("iptables", "-D", "FORWARD", "-i", tapName, "-j", "DROP")
	}

	return hostNetworkConfig{
		TapName:         tapName,
		HostIP:          hostIP,
		GuestIP:         guestIP,
		GuestDNS:        hostIP,
		PolicyResolveMS: 0,
	}, cleanup, nil
}

func isTapDeleteCommand(args []string, tapName string) bool {
	return len(args) == 4 && args[0] == "ip" && args[1] == "link" && args[2] == "del" && args[3] == tapName
}

func deleteTapDeviceWithRetry(ctx context.Context, tapName string, retryInterval time.Duration, interfaceByName interfaceLookupFunc, runner privilegedCommandRunner) error {
	if strings.TrimSpace(tapName) == "" {
		return nil
	}
	if retryInterval <= 0 {
		retryInterval = time.Millisecond
	}

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	for {
		err := runner.Run(ctx, "ip", "link", "del", tapName)
		if err == nil {
			return nil
		}
		if _, lookupErr := interfaceByName(tapName); lookupErr != nil {
			if isNoSuchNetworkInterfaceError(lookupErr) {
				return nil
			}
			return fmt.Errorf("lookup tap device %s after delete failure (%v): %w", tapName, err, lookupErr)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("delete tap device %s: %w", tapName, err)
		case <-ticker.C:
		}
	}
}

func isNoSuchNetworkInterfaceError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		err = opErr.Err
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such network interface") || strings.Contains(msg, "not found")
}

func installForwardReturnPathRule(setupRun func(args ...string) error, tapName string) ([]string, error) {
	return installForwardEstablishedRule(setupRun, "-o", tapName)
}

func installForwardEstablishedEgressRule(setupRun func(args ...string) error, tapName string) ([]string, error) {
	return installForwardEstablishedRule(setupRun, "-i", tapName)
}

func installForwardEstablishedRule(setupRun func(args ...string) error, directionFlag, tapName string) ([]string, error) {
	conntrackRule := []string{"iptables", "-A", "FORWARD", directionFlag, tapName, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(conntrackRule...); err == nil {
		cleanup := append([]string{"iptables", "-D", "FORWARD", directionFlag, tapName}, conntrackRule[5:]...)
		return cleanup, nil
	}
	stateRule := []string{"iptables", "-A", "FORWARD", directionFlag, tapName, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	if err := setupRun(stateRule...); err != nil {
		return nil, err
	}
	cleanup := append([]string{"iptables", "-D", "FORWARD", directionFlag, tapName}, stateRule[5:]...)
	return cleanup, nil
}

func trustedDNSPolicy(allowAll bool, allow []policy.AllowRule) *policy.CompiledPolicy {
	copied := make([]policy.AllowRule, 0, len(allow))
	for _, rule := range allow {
		copied = append(copied, policy.AllowRule{
			Host:  rule.Host,
			Ports: append([]int(nil), rule.Ports...),
		})
	}
	networkDefault := "deny"
	if allowAll {
		networkDefault = "allow"
	}
	return &policy.CompiledPolicy{
		Version:        1,
		NetworkDefault: networkDefault,
		Allow:          copied,
	}
}

func literalIPv4AllowRules(allow []policy.AllowRule) []policy.AllowRule {
	out := make([]policy.AllowRule, 0, len(allow))
	for _, rule := range allow {
		addr, err := netip.ParseAddr(strings.TrimSpace(rule.Host))
		if err != nil || !addr.Is4() {
			continue
		}
		out = append(out, policy.AllowRule{
			Host:  addr.String(),
			Ports: append([]int(nil), rule.Ports...),
		})
	}
	return out
}

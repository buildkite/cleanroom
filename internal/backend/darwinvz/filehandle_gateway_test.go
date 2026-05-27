//go:build darwin

package darwinvz

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
	mdns "github.com/miekg/dns"
	logrus "github.com/sirupsen/logrus"
)

func TestStartFileHandleGatewayRespondsToARP(t *testing.T) {
	t.Parallel()

	runDir := mustMkdirShortTempDir(t)
	gateway, err := startFileHandleGateway(context.Background(), fileHandleGatewayConfig{
		RunDir:     runDir,
		SubnetCIDR: "10.233.0.0/24",
		GatewayIP:  "10.233.0.1",
	})
	if err != nil {
		t.Fatalf("startFileHandleGateway returned error: %v", err)
	}
	defer gateway.Close()

	clientPath := filepath.Join(runDir, "client.sock")
	client, err := net.DialUnix("unixgram", &net.UnixAddr{Name: clientPath, Net: "unixgram"}, &net.UnixAddr{Name: gateway.SocketPath(), Net: "unixgram"})
	if err != nil {
		t.Fatalf("DialUnix returned error: %v", err)
	}
	defer client.Close()

	frame := mustDecodeHex(t,
		"ffffffffffff"+
			"5a94efe40cee"+
			"0806"+
			"0001"+
			"0800"+
			"06"+
			"04"+
			"0001"+
			"5a94efe40cee"+
			"0ae90002"+
			"000000000000"+
			"0ae90001",
	)
	if _, err := client.Write([]byte("VFKT")); err != nil {
		t.Fatalf("client.Write handshake returned error: %v", err)
	}
	if _, err := client.Write(frame); err != nil {
		t.Fatalf("client.Write returned error: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("client.Read returned error: %v", err)
	}

	got := hex.EncodeToString(buf[:n])
	want := "5a94efe40cee5a94efe40cdd080600010800060400025a94efe40cdd0ae900015a94efe40cee0ae90002"
	if got != want {
		t.Fatalf("unexpected ARP reply:\n got %s\nwant %s", got, want)
	}
}

func TestFileHandleGatewayCloseRemovesSocketPath(t *testing.T) {
	t.Parallel()

	runDir := mustMkdirShortTempDir(t)
	gateway, err := startFileHandleGateway(context.Background(), fileHandleGatewayConfig{
		RunDir:     runDir,
		SubnetCIDR: "10.233.0.0/24",
		GatewayIP:  "10.233.0.1",
	})
	if err != nil {
		t.Fatalf("startFileHandleGateway returned error: %v", err)
	}

	socketPath := gateway.SocketPath()
	if _, err := net.ResolveUnixAddr("unixgram", socketPath); err != nil {
		t.Fatalf("ResolveUnixAddr(%q) returned error: %v", socketPath, err)
	}
	if err := gateway.Close(); err != nil {
		t.Fatalf("gateway.Close returned error: %v", err)
	}
	if _, err := net.Dial("unixgram", socketPath); err == nil {
		t.Fatalf("expected socket %q to be closed after Close", socketPath)
	}
}

func TestStartFileHandleGatewayDoesNotResolveAllowRulesAtStartup(t *testing.T) {
	t.Parallel()

	runDir := mustMkdirShortTempDir(t)
	gateway, err := startFileHandleGateway(context.Background(), fileHandleGatewayConfig{
		SandboxID:  "sandbox-1",
		RunDir:     runDir,
		SubnetCIDR: "10.233.0.0/24",
		GatewayIP:  "10.233.0.1",
		Policy: &policy.CompiledPolicy{
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "missing.example", Ports: []int{443}},
			},
		},
	})
	if err != nil {
		t.Fatalf("startFileHandleGateway returned error: %v", err)
	}
	if err := gateway.Close(); err != nil {
		t.Fatalf("gateway.Close returned error: %v", err)
	}
}

func TestNewFileHandleDNSRuntimeRequiresSandboxIDWhenPolicyPresent(t *testing.T) {
	t.Parallel()

	_, err := newFileHandleDNSRuntime("", &policy.CompiledPolicy{
		NetworkDefault: "deny",
	})
	if err == nil {
		t.Fatal("expected missing sandbox id to fail")
	}
}

func TestNewFileHandleDNSRuntimeAcceptsAllowDefaultPolicy(t *testing.T) {
	t.Parallel()

	runtime, err := newFileHandleDNSRuntime("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "allow",
	})
	if err != nil {
		t.Fatalf("newFileHandleDNSRuntime returned error: %v", err)
	}
	if !runtime.HostAllowedByPolicy("sandbox-1", "example.com") {
		t.Fatal("expected allow-default policy to allow arbitrary hosts")
	}
}

func TestFileHandleGatewayAllowsOnlyPublicHostDialDestinations(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		addr    netip.Addr
		allowed bool
	}{
		{name: "public IPv4", addr: netip.MustParseAddr("93.184.216.34"), allowed: true},
		{name: "public IPv6", addr: netip.MustParseAddr("2606:4700:4700::1111"), allowed: true},
		{name: "private IPv4", addr: netip.MustParseAddr("10.0.0.1")},
		{name: "private IPv4 mapped", addr: netip.MustParseAddr("::ffff:10.0.0.1")},
		{name: "loopback IPv4", addr: netip.MustParseAddr("127.0.0.1")},
		{name: "link local IPv4", addr: netip.MustParseAddr("169.254.169.254")},
		{name: "carrier grade NAT", addr: netip.MustParseAddr("100.64.0.1")},
		{name: "IETF protocol assignment IPv4", addr: netip.MustParseAddr("192.0.0.9")},
		{name: "documentation IPv4", addr: netip.MustParseAddr("192.0.2.1")},
		{name: "AS112 IPv4", addr: netip.MustParseAddr("192.31.196.1")},
		{name: "AMT IPv4", addr: netip.MustParseAddr("192.52.193.1")},
		{name: "direct delegation AS112 IPv4", addr: netip.MustParseAddr("192.175.48.1")},
		{name: "benchmarking IPv4", addr: netip.MustParseAddr("198.19.0.1")},
		{name: "reserved IPv4", addr: netip.MustParseAddr("240.0.0.1")},
		{name: "loopback IPv6", addr: netip.MustParseAddr("::1")},
		{name: "NAT64 well known IPv6", addr: netip.MustParseAddr("64:ff9b::a00:1")},
		{name: "private IPv6", addr: netip.MustParseAddr("fd00::1")},
		{name: "link local IPv6", addr: netip.MustParseAddr("fe80::1")},
		{name: "discard only IPv6", addr: netip.MustParseAddr("100::1")},
		{name: "dummy IPv6 prefix", addr: netip.MustParseAddr("100:0:0:1::1")},
		{name: "AMT IPv6", addr: netip.MustParseAddr("2001:3::1")},
		{name: "ORCHIDv2 IPv6", addr: netip.MustParseAddr("2001:20::1")},
		{name: "DETs IPv6", addr: netip.MustParseAddr("2001:30::1")},
		{name: "documentation IPv6", addr: netip.MustParseAddr("2001:db8::1")},
		{name: "direct delegation AS112 IPv6", addr: netip.MustParseAddr("2620:4f:8000::1")},
		{name: "unallocated global unicast IPv6", addr: netip.MustParseAddr("3000::1")},
		{name: "outside allocated global unicast IPv6", addr: netip.MustParseAddr("4000::1")},
		{name: "SRv6 SID IPv6", addr: netip.MustParseAddr("5f00::1")},
		{name: "multicast IPv6", addr: netip.MustParseAddr("ff02::1")},
		{name: "invalid"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := fileHandleGatewayAllowsHostDialDestination(tc.addr)
			if got != tc.allowed {
				t.Fatalf("fileHandleGatewayAllowsHostDialDestination(%s) = %t, want %t", tc.addr, got, tc.allowed)
			}
		})
	}
}

func TestFileHandleGatewaySetPolicyUpdatesDNSRuntime(t *testing.T) {
	t.Parallel()

	runtime, err := newFileHandleDNSRuntime("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "old.example", Ports: []int{443}},
		},
	})
	if err != nil {
		t.Fatalf("newFileHandleDNSRuntime returned error: %v", err)
	}
	gateway := &fileHandleGateway{
		network: &fileHandleVirtualNetwork{dnsRuntime: runtime},
	}

	if err := gateway.SetPolicy("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "new.example", Ports: []int{443}},
		},
	}); err != nil {
		t.Fatalf("SetPolicy returned error: %v", err)
	}
	if runtime.HostAllowedByPolicy("sandbox-1", "old.example") {
		t.Fatal("did not expect old policy host to remain allowed")
	}
	if !runtime.HostAllowedByPolicy("sandbox-1", "new.example") {
		t.Fatal("expected new policy host to be allowed")
	}
}

func TestFileHandleVirtualNetworkSetPolicyClosesActiveTCPProxyConnections(t *testing.T) {
	t.Parallel()

	runtime, err := newFileHandleDNSRuntime("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "old.example", Ports: []int{443}},
		},
	})
	if err != nil {
		t.Fatalf("newFileHandleDNSRuntime returned error: %v", err)
	}
	network := &fileHandleVirtualNetwork{dnsRuntime: runtime}
	guest, guestPeer := net.Pipe()
	defer guestPeer.Close()
	outbound, outboundPeer := net.Pipe()
	defer outboundPeer.Close()

	untrack := network.trackTCPProxyConn(guest, outbound)
	if err := network.SetPolicy("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "new.example", Ports: []int{443}},
		},
	}); err != nil {
		t.Fatalf("SetPolicy returned error: %v", err)
	}
	untrack()

	if _, err := guest.Write([]byte("x")); err == nil {
		t.Fatal("expected tracked guest connection to be closed")
	}
	if _, err := outbound.Write([]byte("x")); err == nil {
		t.Fatal("expected tracked outbound connection to be closed")
	}
	if runtime.HostAllowedByPolicy("sandbox-1", "old.example") {
		t.Fatal("did not expect old policy host to remain allowed")
	}
	if !runtime.HostAllowedByPolicy("sandbox-1", "new.example") {
		t.Fatal("expected new policy host to be allowed")
	}
}

func TestFileHandleVirtualNetworkSetPolicyCancelsPendingTCPProxyDial(t *testing.T) {
	t.Parallel()

	runtime, err := newFileHandleDNSRuntime("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "old.example", Ports: []int{443}},
		},
	})
	if err != nil {
		t.Fatalf("newFileHandleDNSRuntime returned error: %v", err)
	}
	network := &fileHandleVirtualNetwork{dnsRuntime: runtime}
	dialCtx, cancelDial := context.WithCancel(context.Background())
	network.activeMu.Lock()
	_, untrack := network.trackTCPProxyConnLocked(cancelDial)
	network.activeMu.Unlock()
	defer untrack()

	if err := network.SetPolicy("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "new.example", Ports: []int{443}},
		},
	}); err != nil {
		t.Fatalf("SetPolicy returned error: %v", err)
	}
	select {
	case <-dialCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending TCP proxy dial to be canceled")
	}
	if runtime.HostAllowedByPolicy("sandbox-1", "old.example") {
		t.Fatal("did not expect old policy host to remain allowed")
	}
	if !runtime.HostAllowedByPolicy("sandbox-1", "new.example") {
		t.Fatal("expected new policy host to be allowed")
	}
}

func TestFileHandleVirtualNetworkSetPolicySerializesWithTCPAdmission(t *testing.T) {
	t.Parallel()

	runtime, err := newFileHandleDNSRuntime("sandbox-1", &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "old.example", Ports: []int{443}},
		},
	})
	if err != nil {
		t.Fatalf("newFileHandleDNSRuntime returned error: %v", err)
	}
	network := &fileHandleVirtualNetwork{dnsRuntime: runtime}
	network.activeMu.Lock()

	done := make(chan error, 1)
	go func() {
		done <- network.SetPolicy("sandbox-1", &policy.CompiledPolicy{
			NetworkDefault: "deny",
			Allow: []policy.AllowRule{
				{Host: "new.example", Ports: []int{443}},
			},
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("SetPolicy completed while TCP admission lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	network.activeMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetPolicy returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SetPolicy after releasing TCP admission lock")
	}
	if runtime.HostAllowedByPolicy("sandbox-1", "old.example") {
		t.Fatal("did not expect old policy host to remain allowed")
	}
	if !runtime.HostAllowedByPolicy("sandbox-1", "new.example") {
		t.Fatal("expected new policy host to be allowed")
	}
}

func TestResolveFileHandleDNSUpstreamAddrUsesConfiguredValue(t *testing.T) {
	t.Parallel()

	got, err := resolveFileHandleDNSUpstreamAddr("9.9.9.9:53")
	if err != nil {
		t.Fatalf("resolveFileHandleDNSUpstreamAddr returned error: %v", err)
	}
	if got != "9.9.9.9:53" {
		t.Fatalf("unexpected configured upstream addr: got %q want %q", got, "9.9.9.9:53")
	}
}

func TestResolveFileHandleDNSUpstreamAddrFallsBackToSystemConfig(t *testing.T) {
	previous := fileHandleDNSClientConfigFromFile
	fileHandleDNSClientConfigFromFile = func(string) (*mdns.ClientConfig, error) {
		return &mdns.ClientConfig{
			Servers: []string{"192.0.2.53"},
			Port:    "5353",
		}, nil
	}
	t.Cleanup(func() {
		fileHandleDNSClientConfigFromFile = previous
	})

	got, err := resolveFileHandleDNSUpstreamAddr("")
	if err != nil {
		t.Fatalf("resolveFileHandleDNSUpstreamAddr returned error: %v", err)
	}
	if got != "192.0.2.53:5353" {
		t.Fatalf("unexpected system upstream addr: got %q want %q", got, "192.0.2.53:5353")
	}
}

func TestNewFileHandleScopeResolverRejectsGatewayIP(t *testing.T) {
	t.Parallel()

	resolver := newFileHandleScopeResolver("sandbox-1", "10.233.0.1")
	if sandboxID, ok := resolver(netip.MustParseAddr("10.233.0.1")); ok || sandboxID != "" {
		t.Fatalf("expected gateway ip to be rejected, got sandbox_id=%q ok=%t", sandboxID, ok)
	}
	if sandboxID, ok := resolver(netip.MustParseAddr("10.233.0.2")); !ok || sandboxID != "sandbox-1" {
		t.Fatalf("expected guest ip to resolve sandbox scope, got sandbox_id=%q ok=%t", sandboxID, ok)
	}
}

func TestFileHandleGatewayHTTPBridgeForwardsScopeToken(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Cleanroom-Scope-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	bridge, err := newFileHandleGatewayHTTPBridge(upstream.URL)
	if err != nil {
		t.Fatalf("newFileHandleGatewayHTTPBridge returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://10.233.0.1:8170/git/github.com/org/repo.git/info/refs", nil)
	resp := httptest.NewRecorder()
	bridge.ServeHTTP(resp, req)
	if got, want := resp.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("unexpected status without scope token: got %d want %d", got, want)
	}

	bridge.SetScopeToken("scope-token")
	resp = httptest.NewRecorder()
	bridge.ServeHTTP(resp, req)
	if got, want := resp.Code, http.StatusNoContent; got != want {
		t.Fatalf("unexpected proxied status: got %d want %d", got, want)
	}
	if got, want := gotPath, "/git/github.com/org/repo.git/info/refs"; got != want {
		t.Fatalf("unexpected proxied path: got %q want %q", got, want)
	}
	if got, want := gotHeader, "scope-token"; got != want {
		t.Fatalf("unexpected proxied scope token header: got %q want %q", got, want)
	}
}

func TestMuteFileHandleGatewayDependencyLogsReferenceCounts(t *testing.T) {
	logger := logrus.StandardLogger()
	originalOut := logger.Out
	originalLevel := logger.Level
	t.Cleanup(func() {
		logger.SetOutput(originalOut)
		logger.SetLevel(originalLevel)
	})

	var sink bytes.Buffer
	logger.SetOutput(&sink)

	restoreOne := muteFileHandleGatewayDependencyLogs()
	if logger.Out != io.Discard {
		t.Fatalf("expected logrus output to be discarded while muted, got %T", logger.Out)
	}

	restoreTwo := muteFileHandleGatewayDependencyLogs()
	restoreOne()
	if logger.Out != io.Discard {
		t.Fatalf("expected second mute to keep logrus output discarded, got %T", logger.Out)
	}

	restoreTwo()
	if logger.Out != &sink {
		t.Fatalf("expected logrus output to be restored after final release")
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()

	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("DecodeString(%q) returned error: %v", value, err)
	}
	return decoded
}

func mustMkdirShortTempDir(t *testing.T) string {
	t.Helper()

	runDir, err := os.MkdirTemp("", "crfh-")
	if err != nil {
		t.Fatalf("MkdirTemp returned error: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	return runDir
}

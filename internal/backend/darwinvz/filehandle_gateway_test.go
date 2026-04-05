//go:build darwin

package darwinvz

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/policy"
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

func TestBuildFileHandleGatewayPolicyResolvesAllowRules(t *testing.T) {
	t.Parallel()

	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "github.com":
			return []netip.Addr{netip.MustParseAddr("140.82.112.4")}, nil
		default:
			return nil, nil
		}
	}

	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "github.com", Ports: []int{443, 443}},
		},
	}

	got, err := buildFileHandleGatewayPolicy(context.Background(), compiled, lookup)
	if err != nil {
		t.Fatalf("buildFileHandleGatewayPolicy returned error: %v", err)
	}
	if !got.allowsTCP(netip.MustParseAddr("140.82.112.4"), 443) {
		t.Fatal("expected github.com:443 to be allowed")
	}
	if got.allowsTCP(netip.MustParseAddr("140.82.112.4"), 80) {
		t.Fatal("expected github.com:80 to be denied")
	}
	if got.allowsTCP(netip.MustParseAddr("140.82.112.5"), 443) {
		t.Fatal("expected non-allowlisted IP to be denied")
	}
}

func TestBuildFileHandleGatewayPolicyRejectsHostWithoutIPv4Resolution(t *testing.T) {
	t.Parallel()

	lookup := func(_ context.Context, _ string) ([]netip.Addr, error) {
		return nil, nil
	}

	compiled := &policy.CompiledPolicy{
		NetworkDefault: "deny",
		Allow: []policy.AllowRule{
			{Host: "missing.example", Ports: []int{443}},
		},
	}

	_, err := buildFileHandleGatewayPolicy(context.Background(), compiled, lookup)
	if err == nil {
		t.Fatal("expected unresolved host to fail")
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

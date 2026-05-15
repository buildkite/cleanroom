package exposure

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/miekg/dns"
)

func TestRegisterTCPRejectsHostPortConflict(t *testing.T) {
	t.Parallel()

	hostPort := freeTCPPort(t)
	manager := NewManager(Config{TCPHost: "127.0.0.1"})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	exposed, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "tcp",
			HostPort:  int32(hostPort),
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if got, want := exposed.GetUrl(), "tcp://127.0.0.1:"+strconv.Itoa(hostPort); got != want {
		t.Fatalf("unexpected exposed URL: got %q want %q", got, want)
	}

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-2",
		SandboxID: "sandbox-2",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "tcp",
			HostPort:  int32(hostPort),
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "already exposed") {
		t.Fatalf("expected clear conflict error, got %v", err)
	}
}

func TestTCPForwardPreservesResponseAfterClientHalfClose(t *testing.T) {
	t.Parallel()

	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backend service: %v", err)
	}
	t.Cleanup(func() {
		_ = backendLn.Close()
	})
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backendLn.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		req, err := io.ReadAll(conn)
		if err != nil {
			backendDone <- err
			return
		}
		if string(req) != "request" {
			backendDone <- fmt.Errorf("unexpected backend request: %q", req)
			return
		}
		_, err = conn.Write([]byte("response"))
		backendDone <- err
	}()

	hostPort := freeTCPPort(t)
	manager := NewManager(Config{TCPHost: "127.0.0.1"})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "tcp",
			HostPort:  int32(hostPort),
			GuestPort: 3000,
		},
		Dialer: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", backendLn.Addr().String())
		},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
	if err != nil {
		t.Fatalf("dial tcp exposure: %v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	closeWriter, ok := client.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("expected tcp client to support CloseWrite")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatalf("half-close client write side: %v", err)
	}
	resp, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got, want := string(resp), "response"; got != want {
		t.Fatalf("unexpected response: got %q want %q", got, want)
	}
	select {
	case err := <-backendDone:
		if err != nil {
			t.Fatalf("backend returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend service")
	}
}

func TestRegisterHTTPSBuildsCleanroomLocalhostRoute(t *testing.T) {
	t.Parallel()

	httpsPort := freeTCPPort(t)
	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(httpsPort)),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	exposed, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if got, want := exposed.GetHostname(), "buildkite.cleanroom.localhost"; got != want {
		t.Fatalf("unexpected hostname: got %q want %q", got, want)
	}
	if got, want := exposed.GetUrl(), "https://buildkite.cleanroom.localhost:"+strconv.Itoa(httpsPort); got != want {
		t.Fatalf("unexpected exposed URL: got %q want %q", got, want)
	}

	manager.ReleaseOwner("owner-1")
	manager.mu.RLock()
	_, ok := manager.httpsRoutes["buildkite.cleanroom.localhost"]
	manager.mu.RUnlock()
	if ok {
		t.Fatal("expected release owner to remove https route")
	}
}

func TestRegisterHTTPSRetriesAfterListenFailure(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve https listen port: %v", err)
	}
	httpsListen := ln.Addr().String()
	manager := NewManager(Config{
		HTTPSListen: httpsListen,
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err == nil {
		t.Fatal("expected listen conflict error")
	}
	if !strings.Contains(err.Error(), "listen https exposure") {
		t.Fatalf("expected listen conflict error, got %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("release reserved https listen port: %v", err)
	}

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-2",
		SandboxID: "sandbox-2",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("expected retry to start https server, got %v", err)
	}
}

func TestRegisterHTTPSFailureKeepsExistingOwnerRoutes(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve https listen port: %v", err)
	}
	defer ln.Close()

	hostPort := freeTCPPort(t)
	manager := NewManager(Config{
		TCPHost:     "127.0.0.1",
		HTTPSListen: ln.Addr().String(),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "tcp",
			HostPort:  int32(hostPort),
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register tcp returned error: %v", err)
	}

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err == nil {
		t.Fatal("expected https listen conflict error")
	}
	if !strings.Contains(err.Error(), "listen https exposure") {
		t.Fatalf("expected listen conflict error, got %v", err)
	}

	manager.mu.RLock()
	tcpRoute := manager.tcpRoutes[hostPort]
	httpsRoute := manager.httpsRoutes["buildkite.cleanroom.localhost"]
	ownerRoutes := append([]*route(nil), manager.byOwner["owner-1"]...)
	manager.mu.RUnlock()
	if tcpRoute == nil {
		t.Fatal("expected failed https registration to keep existing tcp route")
	}
	if httpsRoute != nil {
		t.Fatal("expected failed https registration to roll back only the new https route")
	}
	if len(ownerRoutes) != 1 || ownerRoutes[0] != tcpRoute {
		t.Fatalf("unexpected owner routes after rollback: got %d routes", len(ownerRoutes))
	}
}

func TestRegisterHTTPSFallsBackToEphemeralPortWhenDefaultListenIsBusy(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve https listen port: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	_, reservedPort, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split reserved listen address: %v", err)
	}

	manager := NewManager(Config{
		HTTPSListen:        ln.Addr().String(),
		AllowHTTPSFallback: true,
		TLSDir:             t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	exposed, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	parsed, err := url.Parse(exposed.GetUrl())
	if err != nil {
		t.Fatalf("parse exposed URL: %v", err)
	}
	if got, want := parsed.Hostname(), "buildkite.cleanroom.localhost"; got != want {
		t.Fatalf("unexpected hostname: got %q want %q", got, want)
	}
	if parsed.Port() == "" {
		t.Fatalf("expected fallback URL to include an ephemeral port, got %q", exposed.GetUrl())
	}
	if parsed.Port() == reservedPort {
		t.Fatalf("expected fallback URL to avoid reserved port %s, got %q", reservedPort, exposed.GetUrl())
	}
}

func TestRegisterHTTPSReusesProxyTransport(t *testing.T) {
	t.Parallel()

	httpsPort := freeTCPPort(t)
	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(httpsPort)),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	manager.mu.RLock()
	route := manager.httpsRoutes["buildkite.cleanroom.localhost"]
	manager.mu.RUnlock()
	if route == nil {
		t.Fatal("expected registered https route")
	}
	if route.httpsProxy == nil || route.httpsTransport == nil {
		t.Fatal("expected https route to own a proxy and transport")
	}
	if route.httpsProxy.Transport != route.httpsTransport {
		t.Fatal("expected https route proxy to reuse the route transport")
	}
}

func TestRegisterHTTPSRoutesWildcardLocalhostPattern(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen <- req.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(freeTCPPort(t))),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	exposed, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "*.sandbox-1.localhost",
			GuestPort: 3000,
		},
		Dialer: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", backendURL.Host)
		},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if got, want := exposed.GetHostname(), "*.sandbox-1.localhost"; got != want {
		t.Fatalf("unexpected hostname: got %q want %q", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "https://api.sandbox-1.localhost/", nil)
	req.Host = "api.sandbox-1.localhost"
	rr := httptest.NewRecorder()
	manager.handleHTTPS(rr, req)
	if got, want := rr.Code, http.StatusNoContent; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}
	select {
	case got := <-seen:
		if got != "api.sandbox-1.localhost" {
			t.Fatalf("unexpected backend host: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend request")
	}

	req = httptest.NewRequest(http.MethodGet, "https://deep.api.sandbox-1.localhost/", nil)
	req.Host = "deep.api.sandbox-1.localhost"
	rr = httptest.NewRecorder()
	manager.handleHTTPS(rr, req)
	if got, want := rr.Code, http.StatusNotFound; got != want {
		t.Fatalf("unexpected deep wildcard status: got %d want %d", got, want)
	}
}

func TestHTTPSCertificateGenerationUsesBoundedManagedWildcardFallback(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(freeTCPPort(t))),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	manager.mu.RLock()
	cacheEntries := len(manager.certCache)
	manager.mu.RUnlock()

	cert, err := manager.getHTTPSCertificate(&tls.ClientHelloInfo{ServerName: "missing.cleanroom.localhost"})
	if err != nil {
		t.Fatalf("expected one-label managed hostname to use wildcard fallback certificate: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("expected fallback certificate to include parsed leaf")
	}
	if err := cert.Leaf.VerifyHostname("missing.cleanroom.localhost"); err != nil {
		t.Fatalf("expected fallback certificate to verify missing hostname: %v", err)
	}
	manager.mu.RLock()
	if got, want := len(manager.certCache), cacheEntries+1; got != want {
		manager.mu.RUnlock()
		t.Fatalf("expected one wildcard fallback certificate cache entry: got %d want %d", got, want)
	}
	if manager.certCache["*.cleanroom.localhost"] == nil {
		manager.mu.RUnlock()
		t.Fatal("expected wildcard fallback certificate to be cached by pattern")
	}
	manager.mu.RUnlock()

	_, err = manager.getHTTPSCertificate(&tls.ClientHelloInfo{ServerName: "deep.missing.cleanroom.localhost"})
	if err == nil {
		t.Fatal("expected unregistered deep managed hostname to be rejected")
	}
	if !strings.Contains(err.Error(), "unhandled") {
		t.Fatalf("unexpected certificate error: %v", err)
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if got, want := len(manager.certCache), cacheEntries+1; got != want {
		t.Fatalf("expected rejected deep hostname not to populate certificate cache: got %d want %d", got, want)
	}
}

func TestHTTPSCertificateCacheLimitBoundsWildcardRouteCertificates(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(freeTCPPort(t))),
		TLSDir:      t.TempDir(),
	})
	manager.certCacheLimit = 3
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "*.*.sandbox-1.localhost",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	for _, name := range []string{"a.b.sandbox-1.localhost", "c.d.sandbox-1.localhost"} {
		if _, err := manager.getHTTPSCertificate(&tls.ClientHelloInfo{ServerName: name}); err != nil {
			t.Fatalf("getHTTPSCertificate(%q) returned error: %v", name, err)
		}
	}
	_, err = manager.getHTTPSCertificate(&tls.ClientHelloInfo{ServerName: "e.f.sandbox-1.localhost"})
	if err == nil {
		t.Fatal("expected certificate cache limit error")
	}
	if !strings.Contains(err.Error(), "cache is full") {
		t.Fatalf("unexpected certificate error: %v", err)
	}

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-2",
		SandboxID: "sandbox-2",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err != nil {
		t.Fatalf("Register exact route returned error: %v", err)
	}
	if _, err := manager.getHTTPSCertificate(&tls.ClientHelloInfo{ServerName: "buildkite.cleanroom.localhost"}); err != nil {
		t.Fatalf("expected exact route certificate to bypass wildcard cache limit, got %v", err)
	}
}

func TestRegisterHTTPSRejectsExternalRouteHost(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{TLSDir: t.TempDir()})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "google.com",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	})
	if err == nil {
		t.Fatal("expected external host to be rejected")
	}
	if !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPSProxyForwardsTrustedHeadersAndPreservesHost(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		host             string
		xForwardedHost   string
		xForwardedProto  string
		xForwardedPort   string
		xForwardedFor    string
		clientHeaderSeen string
	}

	seen := make(chan observedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seen <- observedRequest{
			host:             req.Host,
			xForwardedHost:   req.Header.Get("X-Forwarded-Host"),
			xForwardedProto:  req.Header.Get("X-Forwarded-Proto"),
			xForwardedPort:   req.Header.Get("X-Forwarded-Port"),
			xForwardedFor:    req.Header.Get("X-Forwarded-For"),
			clientHeaderSeen: req.Header.Get("X-Client-Test"),
		}
		w.Header().Set("Location", "https://"+req.Host+"/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	httpsPort := freeTCPPort(t)
	manager := NewManager(Config{
		HTTPSListen: net.JoinHostPort("127.0.0.1", strconv.Itoa(httpsPort)),
		TLSDir:      t.TempDir(),
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})

	_, err = manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "buildkite",
			GuestPort: 3000,
		},
		Dialer: func(ctx context.Context, _ string, _ int) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", backendURL.Host)
		},
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://buildkite.cleanroom.localhost:8143/sessions", nil)
	req.Host = "buildkite.cleanroom.localhost:8143"
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-Host", "malicious.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-Port", "9999")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("X-Client-Test", "kept")

	rr := httptest.NewRecorder()
	manager.handleHTTPS(rr, req)

	if got, want := rr.Code, http.StatusFound; got != want {
		t.Fatalf("unexpected status: got %d want %d", got, want)
	}
	if got, want := rr.Header().Get("Location"), "https://buildkite.cleanroom.localhost:8143/redirected"; got != want {
		t.Fatalf("unexpected redirect location: got %q want %q", got, want)
	}

	select {
	case got := <-seen:
		if got.host != "buildkite.cleanroom.localhost:8143" {
			t.Fatalf("unexpected host: %q", got.host)
		}
		if got.xForwardedHost != "buildkite.cleanroom.localhost:8143" {
			t.Fatalf("unexpected X-Forwarded-Host: %q", got.xForwardedHost)
		}
		if got.xForwardedProto != "https" {
			t.Fatalf("unexpected X-Forwarded-Proto: %q", got.xForwardedProto)
		}
		if got.xForwardedPort != "8143" {
			t.Fatalf("unexpected X-Forwarded-Port: %q", got.xForwardedPort)
		}
		if got.xForwardedFor != "127.0.0.1" {
			t.Fatalf("unexpected X-Forwarded-For: %q", got.xForwardedFor)
		}
		if got.clientHeaderSeen != "kept" {
			t.Fatalf("unexpected preserved header value: %q", got.clientHeaderSeen)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend request")
	}
}

func TestDNSReturnsNoErrorForKnownNonAQueries(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	msg := new(dns.Msg)
	msg.SetQuestion("buildkite.cleanroom.localhost.", dns.TypeAAAA)
	w := &captureDNSResponseWriter{}
	manager.handleDNS(w, msg)

	if w.msg == nil {
		t.Fatal("expected DNS response")
	}
	if got, want := w.msg.Rcode, dns.RcodeSuccess; got != want {
		t.Fatalf("unexpected DNS rcode: got %d want %d", got, want)
	}
	if len(w.msg.Answer) != 0 {
		t.Fatalf("expected no AAAA answers, got %v", w.msg.Answer)
	}
}

func TestDNSReturnsLoopbackForWildcardNames(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	msg := new(dns.Msg)
	msg.SetQuestion("missing.cleanroom.localhost.", dns.TypeA)
	w := &captureDNSResponseWriter{}
	manager.handleDNS(w, msg)

	if w.msg == nil {
		t.Fatal("expected DNS response")
	}
	if got, want := w.msg.Rcode, dns.RcodeSuccess; got != want {
		t.Fatalf("unexpected DNS rcode: got %d want %d", got, want)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected one A answer, got %v", w.msg.Answer)
	}
	a, ok := w.msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("expected A answer, got %T", w.msg.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("unexpected A answer: got %v", a.A)
	}
}

func TestDNSReturnsLoopbackForConfiguredLocalhostRoute(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{TLSDir: t.TempDir()})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	if _, err := manager.Register(context.Background(), RegisterRequest{
		OwnerID:   "owner-1",
		SandboxID: "sandbox-1",
		Exposure: &cleanroomv1.PortExposure{
			Protocol:  "https",
			Name:      "*.sandbox-1.localhost",
			GuestPort: 3000,
		},
		Dialer: testDialer,
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	msg := new(dns.Msg)
	msg.SetQuestion("api.sandbox-1.localhost.", dns.TypeA)
	w := &captureDNSResponseWriter{}
	manager.handleDNS(w, msg)

	if w.msg == nil {
		t.Fatal("expected DNS response")
	}
	if got, want := w.msg.Rcode, dns.RcodeSuccess; got != want {
		t.Fatalf("unexpected DNS rcode: got %d want %d", got, want)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected one A answer, got %v", w.msg.Answer)
	}
}

func TestStartDNSReusesExistingWildcardDNS(t *testing.T) {
	t.Parallel()

	listen := freeDNSListen(t)
	manager := NewManager(Config{DNSListen: listen})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	if err := manager.StartDNS(context.Background()); err != nil {
		t.Fatalf("StartDNS returned error: %v", err)
	}

	second := NewManager(Config{DNSListen: listen})
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Close returned error: %v", err)
		}
	})
	if err := second.StartDNS(context.Background()); err != nil {
		t.Fatalf("second StartDNS should reuse existing DNS listener, got %v", err)
	}

	msg := new(dns.Msg)
	msg.SetQuestion("other.cleanroom.localhost.", dns.TypeA)
	resp, _, err := (&dns.Client{Net: "udp"}).Exchange(msg, listen)
	if err != nil {
		t.Fatalf("query existing DNS listener: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("expected wildcard DNS answer, got rcode=%d answers=%v", resp.Rcode, resp.Answer)
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("Close first manager returned error: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		resp, _, err = (&dns.Client{Net: "udp"}).Exchange(msg, listen)
		if err == nil && resp != nil && resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second DNS manager did not take over listener, last err=%v resp=%v", err, resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDNSReturnsNXDOMAINForNamesOutsideDomain(t *testing.T) {
	t.Parallel()

	manager := NewManager(Config{})

	msg := new(dns.Msg)
	msg.SetQuestion("missing.example.com.", dns.TypeA)
	w := &captureDNSResponseWriter{}
	manager.handleDNS(w, msg)

	if w.msg == nil {
		t.Fatal("expected DNS response")
	}
	if got, want := w.msg.Rcode, dns.RcodeNameError; got != want {
		t.Fatalf("unexpected DNS rcode: got %d want %d", got, want)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on free TCP port: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listen address %q: %v", ln.Addr().String(), err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse listen port %q: %v", port, err)
	}
	return n
}

func freeDNSListen(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on free UDP port: %v", err)
	}
	addr := pc.LocalAddr().(*net.UDPAddr)
	if err := pc.Close(); err != nil {
		t.Fatalf("close UDP listener: %v", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
}

func testDialer(context.Context, string, int) (net.Conn, error) {
	return nil, io.EOF
}

type captureDNSResponseWriter struct {
	msg *dns.Msg
}

func (w *captureDNSResponseWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{}
}

func (w *captureDNSResponseWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{}
}

func (w *captureDNSResponseWriter) WriteMsg(msg *dns.Msg) error {
	w.msg = msg.Copy()
	return nil
}

func (w *captureDNSResponseWriter) Write(data []byte) (int, error) {
	msg := new(dns.Msg)
	if err := msg.Unpack(data); err != nil {
		return 0, err
	}
	w.msg = msg
	return len(data), nil
}

func (w *captureDNSResponseWriter) Close() error {
	return nil
}

func (w *captureDNSResponseWriter) TsigStatus() error {
	return nil
}

func (w *captureDNSResponseWriter) TsigTimersOnly(bool) {}

func (w *captureDNSResponseWriter) Hijack() {}

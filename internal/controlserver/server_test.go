package controlserver

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/charmbracelet/log"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

func TestStreamSubscriberDroppedErrWhileExecutionStillRunning(t *testing.T) {
	done := make(chan struct{})

	err := streamSubscriberDroppedErr(done, "execution")
	if err == nil {
		t.Fatal("expected error when stream subscriber is dropped before done")
	}
	if got, want := connect.CodeOf(err), connect.CodeResourceExhausted; got != want {
		t.Fatalf("unexpected connect code: got %v want %v", got, want)
	}
}

func TestStreamSubscriberDroppedErrAfterExecutionDone(t *testing.T) {
	done := make(chan struct{})
	close(done)

	if err := streamSubscriberDroppedErr(done, "execution"); err != nil {
		t.Fatalf("expected nil when stream closes after done, got %v", err)
	}
}

type stubTSNetServer struct {
	listener  net.Listener
	listenErr error

	listenNetwork string
	listenAddr    string
	closeCalls    int
}

func (s *stubTSNetServer) Listen(network, addr string) (net.Listener, error) {
	s.listenNetwork = network
	s.listenAddr = addr
	if s.listenErr != nil {
		return nil, s.listenErr
	}
	if s.listener == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		s.listener = ln
	}
	return s.listener, nil
}

func (s *stubTSNetServer) Close() error {
	s.closeCalls++
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func TestListenTSNetUsesStateDirAndCleanup(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	stub := &stubTSNetServer{}
	var gotStateDir string

	originalNewTSNetServer := newTSNetServer
	newTSNetServer = func(_ endpoint.Endpoint, stateDir string, _ func(format string, args ...any)) tsnetServer {
		gotStateDir = stateDir
		return stub
	}
	t.Cleanup(func() {
		newTSNetServer = originalNewTSNetServer
	})

	ep := endpoint.Endpoint{
		Scheme:        "tsnet",
		Address:       ":7777",
		TSNetHostname: "cleanroom",
	}

	ln, cleanup, err := listen(ep, nil)
	if err != nil {
		t.Fatalf("listen tsnet: %v", err)
	}
	if ln == nil {
		t.Fatal("expected listener")
	}
	if cleanup == nil {
		t.Fatal("expected cleanup callback")
	}
	if stub.listenNetwork != "tcp" {
		t.Fatalf("expected tcp network, got %q", stub.listenNetwork)
	}
	if stub.listenAddr != ":7777" {
		t.Fatalf("expected tsnet listen addr :7777, got %q", stub.listenAddr)
	}

	expectedStateDir := filepath.Join(stateHome, "cleanroom", "tsnet")
	if gotStateDir != expectedStateDir {
		t.Fatalf("expected tsnet state dir %q, got %q", expectedStateDir, gotStateDir)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if stub.closeCalls != 1 {
		t.Fatalf("expected cleanup to close tsnet server once, got %d", stub.closeCalls)
	}
}

func TestListenTSNetClosesServerOnListenError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	stub := &stubTSNetServer{listenErr: errors.New("boom")}

	originalNewTSNetServer := newTSNetServer
	newTSNetServer = func(_ endpoint.Endpoint, _ string, _ func(format string, args ...any)) tsnetServer {
		return stub
	}
	t.Cleanup(func() {
		newTSNetServer = originalNewTSNetServer
	})

	_, cleanup, err := listen(endpoint.Endpoint{
		Scheme:        "tsnet",
		Address:       ":7777",
		TSNetHostname: "cleanroom",
	}, nil)
	if err == nil {
		t.Fatal("expected listen error")
	}
	if cleanup != nil {
		t.Fatal("expected no cleanup callback on listen failure")
	}
	if stub.closeCalls != 1 {
		t.Fatalf("expected listen failure to close tsnet server, got %d", stub.closeCalls)
	}
}

type stubTailscaleLocalClient struct {
	status    *ipnstate.Status
	statusErr error

	serveConfig    *ipn.ServeConfig
	serveConfigErr error

	setServeConfigErr   error
	setServeConfigArg   *ipn.ServeConfig
	setServeConfigCalls int

	prefs       *ipn.Prefs
	getPrefsErr error

	editPrefsErr   error
	editPrefsArg   *ipn.MaskedPrefs
	editPrefsCalls int
}

func (s *stubTailscaleLocalClient) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	return s.status, s.statusErr
}

func (s *stubTailscaleLocalClient) GetServeConfig(context.Context) (*ipn.ServeConfig, error) {
	return s.serveConfig, s.serveConfigErr
}

func (s *stubTailscaleLocalClient) SetServeConfig(_ context.Context, config *ipn.ServeConfig) error {
	s.setServeConfigCalls++
	if config != nil {
		s.setServeConfigArg = config.Clone()
	}
	return s.setServeConfigErr
}

func (s *stubTailscaleLocalClient) GetPrefs(context.Context) (*ipn.Prefs, error) {
	return s.prefs, s.getPrefsErr
}

func (s *stubTailscaleLocalClient) EditPrefs(_ context.Context, prefs *ipn.MaskedPrefs) (*ipn.Prefs, error) {
	s.editPrefsCalls++
	if prefs != nil {
		cp := *prefs
		cp.Prefs.AdvertiseServices = append([]string{}, prefs.Prefs.AdvertiseServices...)
		s.editPrefsArg = &cp
	}
	return s.prefs, s.editPrefsErr
}

func TestListenTailscaleServiceConfiguresServeAndAdvertise(t *testing.T) {
	stub := &stubTailscaleLocalClient{
		status: &ipnstate.Status{
			CurrentTailnet: &ipnstate.TailnetStatus{
				MagicDNSSuffix: "example.ts.net",
			},
		},
		serveConfig: &ipn.ServeConfig{},
		prefs: &ipn.Prefs{
			AdvertiseServices: []string{"svc:existing"},
		},
	}

	originalNewTailscaleLocalClient := newTailscaleLocalClient
	newTailscaleLocalClient = func() tailscaleLocalClient {
		return stub
	}
	t.Cleanup(func() {
		newTailscaleLocalClient = originalNewTailscaleLocalClient
	})

	ep := endpoint.Endpoint{
		Scheme:        "tssvc",
		Address:       "127.0.0.1:0",
		TSServiceName: "svc:cleanroom",
	}

	ln, cleanup, err := listen(ep, nil)
	if err != nil {
		t.Fatalf("listen tailscale service: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})
	if cleanup != nil {
		t.Fatal("expected no cleanup callback for tailscale service listener")
	}

	if stub.setServeConfigCalls != 1 {
		t.Fatalf("expected serve config update call, got %d", stub.setServeConfigCalls)
	}
	if stub.setServeConfigArg == nil {
		t.Fatal("expected serve config argument")
	}

	serviceName := tailcfg.ServiceName("svc:cleanroom")
	serviceConfig := stub.setServeConfigArg.Services[serviceName]
	if serviceConfig == nil {
		t.Fatalf("expected service config for %q", serviceName)
	}
	handler := serviceConfig.TCP[443]
	if handler == nil || !handler.HTTPS {
		t.Fatalf("expected https handler on service port 443, got %+v", handler)
	}

	hostPort := ipn.HostPort("cleanroom.example.ts.net:443")
	webConfig := serviceConfig.Web[hostPort]
	if webConfig == nil {
		t.Fatalf("expected web config for %q", hostPort)
	}
	root := webConfig.Handlers["/"]
	if root == nil {
		t.Fatalf("expected root proxy handler for %q", hostPort)
	}
	expectedProxy := "http://" + ln.Addr().String()
	if root.Proxy != expectedProxy {
		t.Fatalf("expected proxy target %q, got %q", expectedProxy, root.Proxy)
	}

	if stub.editPrefsCalls != 1 {
		t.Fatalf("expected one advertise service update call, got %d", stub.editPrefsCalls)
	}
	if stub.editPrefsArg == nil {
		t.Fatal("expected edit prefs argument")
	}
	gotAdvertise := stub.editPrefsArg.Prefs.AdvertiseServices
	wantAdvertise := []string{"svc:existing", "svc:cleanroom"}
	if !reflect.DeepEqual(gotAdvertise, wantAdvertise) {
		t.Fatalf("expected advertised services %v, got %v", wantAdvertise, gotAdvertise)
	}
}

func TestListenTailscaleServiceSkipsAdvertiseWhenAlreadyPresent(t *testing.T) {
	stub := &stubTailscaleLocalClient{
		status: &ipnstate.Status{
			CurrentTailnet: &ipnstate.TailnetStatus{
				MagicDNSSuffix: "example.ts.net",
			},
		},
		serveConfig: &ipn.ServeConfig{},
		prefs: &ipn.Prefs{
			AdvertiseServices: []string{"svc:cleanroom"},
		},
	}

	originalNewTailscaleLocalClient := newTailscaleLocalClient
	newTailscaleLocalClient = func() tailscaleLocalClient {
		return stub
	}
	t.Cleanup(func() {
		newTailscaleLocalClient = originalNewTailscaleLocalClient
	})

	ln, _, err := listen(endpoint.Endpoint{
		Scheme:        "tssvc",
		Address:       "127.0.0.1:0",
		TSServiceName: "svc:cleanroom",
	}, nil)
	if err != nil {
		t.Fatalf("listen tailscale service: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	if stub.editPrefsCalls != 0 {
		t.Fatalf("expected no advertise update call, got %d", stub.editPrefsCalls)
	}
}

func TestListenTailscaleServiceReturnsErrorWhenStatusFails(t *testing.T) {
	stub := &stubTailscaleLocalClient{
		statusErr: errors.New("tailscaled unavailable"),
	}

	originalNewTailscaleLocalClient := newTailscaleLocalClient
	newTailscaleLocalClient = func() tailscaleLocalClient {
		return stub
	}
	t.Cleanup(func() {
		newTailscaleLocalClient = originalNewTailscaleLocalClient
	})

	ln, cleanup, err := listen(endpoint.Endpoint{
		Scheme:        "tssvc",
		Address:       "127.0.0.1:0",
		TSServiceName: "svc:cleanroom",
	}, nil)
	if err == nil {
		t.Fatal("expected listen to fail when tailscale status is unavailable")
	}
	if ln != nil {
		t.Fatal("expected no listener on tailscale status failure")
	}
	if cleanup != nil {
		t.Fatal("expected no cleanup callback on tailscale status failure")
	}
	if stub.setServeConfigCalls != 0 {
		t.Fatalf("expected no serve config updates, got %d", stub.setServeConfigCalls)
	}
	if stub.editPrefsCalls != 0 {
		t.Fatalf("expected no prefs updates, got %d", stub.editPrefsCalls)
	}
}

func TestListenTSNetWiresLogger(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	stub := &stubTSNetServer{}
	var gotLogf func(format string, args ...any)

	originalNewTSNetServer := newTSNetServer
	newTSNetServer = func(_ endpoint.Endpoint, _ string, tsLogf func(format string, args ...any)) tsnetServer {
		gotLogf = tsLogf
		return stub
	}
	t.Cleanup(func() {
		newTSNetServer = originalNewTSNetServer
	})

	logger := log.NewWithOptions(io.Discard, log.Options{
		Level:     log.DebugLevel,
		Formatter: log.TextFormatter,
	})

	ln, cleanup, err := listen(endpoint.Endpoint{
		Scheme:        "tsnet",
		Address:       ":7777",
		TSNetHostname: "cleanroom",
	}, logger)
	if err != nil {
		t.Fatalf("listen tsnet: %v", err)
	}
	if ln == nil {
		t.Fatal("expected listener")
	}
	if cleanup == nil {
		t.Fatal("expected cleanup callback")
	}
	if gotLogf == nil {
		t.Fatal("expected tsnet log callback to be wired")
	}
	gotLogf("tsnet test message %s", "ok")
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

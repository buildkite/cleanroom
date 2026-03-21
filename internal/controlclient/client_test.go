package controlclient

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/tlsconfig"
	"golang.org/x/net/http2"
)

func TestBuildTransportHTTPSLoadsDiscoveredCA(t *testing.T) {
	t.Helper()

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeTLSMaterial(t, configHome, net.ParseIP("127.0.0.1"))

	rt, err := buildTransport(endpoint.Endpoint{Scheme: "https"}, "https://127.0.0.1:8443", tlsconfig.Options{})
	if err != nil {
		t.Fatalf("buildTransport returned error: %v", err)
	}

	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if got, want := transport.TLSClientConfig.MinVersion, uint16(tls.VersionTLS13); got != want {
		t.Fatalf("unexpected min TLS version: got %d want %d", got, want)
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected discovered client CA pool")
	}
}

func TestBuildTransportUnixUsesHTTP2Transport(t *testing.T) {
	t.Helper()

	rt, err := buildTransport(endpoint.Endpoint{
		Scheme:  "unix",
		Address: "/tmp/cleanroom.sock",
	}, "http://unix", tlsconfig.Options{})
	if err != nil {
		t.Fatalf("buildTransport returned error: %v", err)
	}
	if _, ok := rt.(*http2.Transport); !ok {
		t.Fatalf("expected *http2.Transport, got %T", rt)
	}
}

func TestBuildTransportHTTPUsesHTTP2Transport(t *testing.T) {
	t.Helper()

	rt, err := buildTransport(endpoint.Endpoint{Scheme: "http"}, "http://127.0.0.1:8080", tlsconfig.Options{})
	if err != nil {
		t.Fatalf("buildTransport returned error: %v", err)
	}
	if _, ok := rt.(*http2.Transport); !ok {
		t.Fatalf("expected *http2.Transport, got %T", rt)
	}
}

func TestBuildTransportFallsBackToHTTPTransportOnInvalidBaseURL(t *testing.T) {
	t.Helper()

	rt, err := buildTransport(endpoint.Endpoint{Scheme: "http"}, "://not-a-url", tlsconfig.Options{})
	if err != nil {
		t.Fatalf("buildTransport returned error: %v", err)
	}
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
}

package controlserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/buildkite/cleanroom/internal/endpoint"
	"github.com/buildkite/cleanroom/internal/tlsbootstrap"
)

func TestHTTPSListenerWithMTLS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := tlsbootstrap.Init(dir, false); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "127.0.0.1:0",
		BaseURL: "https://127.0.0.1:0",
	}

	ln, _, err := listen(ep, nil, &TLSOptions{
		CertPath: dir + "/server.pem",
		KeyPath:  dir + "/server.key",
		CAPath:   dir + "/ca.pem",
	})
	if err != nil {
		t.Fatalf("listen https: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	addr := ln.Addr().String()

	t.Run("valid client cert succeeds", func(t *testing.T) {
		client := mtlsHTTPClient(t, dir, dir+"/client.pem", dir+"/client.key")
		resp, err := client.Get(fmt.Sprintf("https://%s/healthz", addr))
		if err != nil {
			t.Fatalf("request with valid client cert: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("no client cert rejected", func(t *testing.T) {
		client := tlsHTTPClientNoClientCert(t, dir)
		_, err := client.Get(fmt.Sprintf("https://%s/healthz", addr))
		if err == nil {
			t.Fatal("expected TLS handshake failure without client cert")
		}
	})

	t.Run("wrong CA rejected", func(t *testing.T) {
		wrongDir := t.TempDir()
		if err := tlsbootstrap.Init(wrongDir, false); err != nil {
			t.Fatalf("tls init (wrong CA): %v", err)
		}
		client := mtlsHTTPClient(t, wrongDir, wrongDir+"/client.pem", wrongDir+"/client.key")
		_, err := client.Get(fmt.Sprintf("https://%s/healthz", addr))
		if err == nil {
			t.Fatal("expected TLS handshake failure with wrong CA")
		}
	})
}

func TestHTTPSListenerWithoutCA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := tlsbootstrap.Init(dir, false); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "127.0.0.1:0",
		BaseURL: "https://127.0.0.1:0",
	}

	// No CAPath — server TLS without mTLS (no client cert required).
	ln, _, err := listen(ep, nil, &TLSOptions{
		CertPath: dir + "/server.pem",
		KeyPath:  dir + "/server.key",
	})
	if err != nil {
		t.Fatalf("listen https: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	addr := ln.Addr().String()

	client := tlsHTTPClientNoClientCert(t, dir)
	resp, err := client.Get(fmt.Sprintf("https://%s/healthz", addr))
	if err != nil {
		t.Fatalf("request without client cert (non-mTLS server): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHTTPSListenerFailsWithoutCerts(t *testing.T) {
	t.Parallel()

	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "127.0.0.1:0",
		BaseURL: "https://127.0.0.1:0",
	}

	_, _, err := listen(ep, nil, &TLSOptions{})
	if err == nil {
		t.Fatal("expected error when no TLS certs are available")
	}
}

func TestServeHTTPSEndToEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := tlsbootstrap.Init(dir, false); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "127.0.0.1:0",
		BaseURL: "https://127.0.0.1:0",
	}

	ln, _, err := listen(ep, nil, &TLSOptions{
		CertPath: dir + "/server.pem",
		KeyPath:  dir + "/server.key",
		CAPath:   dir + "/ca.pem",
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	handler := New(nil, nil).Handler()
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	addr := ln.Addr().String()

	client := mtlsHTTPClient(t, dir, dir+"/client.pem", dir+"/client.key")
	resp, err := client.Get(fmt.Sprintf("https://%s/healthz", addr))
	if err != nil {
		t.Fatalf("healthz via mTLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestServeAcceptsTLSOptions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := tlsbootstrap.Init(dir, false); err != nil {
		t.Fatalf("tls init: %v", err)
	}

	ep := endpoint.Endpoint{
		Scheme:  "https",
		Address: "127.0.0.1:0",
		BaseURL: "https://127.0.0.1:0",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := New(nil, nil).Handler()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, ep, handler, nil, &TLSOptions{
			CertPath: dir + "/server.pem",
			KeyPath:  dir + "/server.key",
			CAPath:   dir + "/ca.pem",
		})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

func mtlsHTTPClient(t *testing.T, caDir, certPath, keyPath string) *http.Client {
	t.Helper()

	caPool := testLoadCAPool(t, caDir+"/ca.pem")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}

	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      caPool,
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS13,
			},
		},
	}
}

func tlsHTTPClientNoClientCert(t *testing.T, caDir string) *http.Client {
	t.Helper()

	caPool := testLoadCAPool(t, caDir+"/ca.pem")
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS13,
			},
		},
	}
}

func testLoadCAPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	caPEM, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("no valid certs in CA file")
	}
	return pool
}

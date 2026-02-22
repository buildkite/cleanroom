// Package tlsconfig discovers and loads TLS material for cleanroom mTLS.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"github.com/buildkite/cleanroom/internal/paths"
)

// Options holds explicit TLS paths from CLI flags or environment variables.
type Options struct {
	CertPath string
	KeyPath  string
	CAPath   string
}

// ResolveServer returns a tls.Config for the server side. If no explicit paths
// are provided, it auto-discovers from the XDG TLS directory.
// Returns nil if no TLS material is found.
func ResolveServer(opts Options) (*tls.Config, error) {
	certPath, keyPath, caPath, err := resolvePaths(opts, "server")
	if err != nil {
		return nil, err
	}
	if certPath == "" || keyPath == "" {
		return nil, nil
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}

// ResolveClient returns a tls.Config for the client side. If no explicit paths
// are provided, it auto-discovers from the XDG TLS directory.
// Returns nil if no TLS material is found.
func ResolveClient(opts Options) (*tls.Config, error) {
	certPath, keyPath, caPath, err := resolvePaths(opts, "client")
	if err != nil {
		return nil, err
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}

	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}

	if certPath != "" && keyPath != "" {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

func resolvePaths(opts Options, role string) (certPath, keyPath, caPath string, err error) {
	certPath = firstNonEmpty(opts.CertPath, os.Getenv("CLEANROOM_TLS_CERT"))
	keyPath = firstNonEmpty(opts.KeyPath, os.Getenv("CLEANROOM_TLS_KEY"))
	caPath = firstNonEmpty(opts.CAPath, os.Getenv("CLEANROOM_TLS_CA"))

	if certPath != "" && keyPath != "" {
		return certPath, keyPath, caPath, nil
	}

	// Auto-discover from XDG TLS directory.
	tlsDir, err := paths.TLSDir()
	if err != nil {
		return "", "", "", nil
	}

	if certPath == "" {
		candidate := filepath.Join(tlsDir, role+".pem")
		if fileExists(candidate) {
			certPath = candidate
		}
	}
	if keyPath == "" {
		candidate := filepath.Join(tlsDir, role+".key")
		if fileExists(candidate) {
			keyPath = candidate
		}
	}
	if caPath == "" {
		candidate := filepath.Join(tlsDir, "ca.pem")
		if fileExists(candidate) {
			caPath = candidate
		}
	}

	return certPath, keyPath, caPath, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", path)
	}
	return pool, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

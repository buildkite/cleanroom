// Package tlsconfig discovers and loads TLS material for cleanroom server-auth TLS.
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
	certPath, keyPath, err := resolveServerPaths(opts)
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
		NextProtos:   []string{"h2", "http/1.1"},
	}

	return tlsCfg, nil
}

// ResolveClient returns a tls.Config for the client side. If no explicit paths
// are provided, it auto-discovers from the XDG TLS directory.
// Returns nil if no TLS material is found.
func ResolveClient(opts Options) (*tls.Config, error) {
	if opts.CertPath != "" || opts.KeyPath != "" {
		return nil, fmt.Errorf("client certificates are not supported")
	}

	caPath, err := resolveClientCAPath(opts.CAPath)
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

	return tlsCfg, nil
}

func resolveServerPaths(opts Options) (certPath, keyPath string, err error) {
	certPath = opts.CertPath
	keyPath = opts.KeyPath
	tlsDir, dirErr := paths.TLSDir()
	if dirErr != nil {
		return certPath, keyPath, nil
	}

	if certPath == "" {
		candidate := filepath.Join(tlsDir, "server.pem")
		if fileExists(candidate) {
			certPath = candidate
		}
	}
	if keyPath == "" {
		candidate := filepath.Join(tlsDir, "server.key")
		if fileExists(candidate) {
			keyPath = candidate
		}
	}

	return certPath, keyPath, nil
}

func resolveClientCAPath(caPath string) (string, error) {
	if caPath != "" {
		return caPath, nil
	}

	tlsDir, dirErr := paths.TLSDir()
	if dirErr != nil {
		return "", nil
	}

	candidate := filepath.Join(tlsDir, "ca.pem")
	if fileExists(candidate) {
		return candidate, nil
	}

	return "", nil
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

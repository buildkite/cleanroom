package exposure

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

const (
	LocalCertificateFilename    = "exposure-cert.pem"
	LocalCertificateKeyFilename = "exposure-cert.key"
)

var ErrLocalCertificateRequiresInstall = errors.New("exposure certificate must be refreshed with cleanroom dns install")

type LocalCertificate struct {
	Cert     *x509.Certificate
	Key      *rsa.PrivateKey
	CertPEM  []byte
	CertPath string
	KeyPath  string
}

func DefaultTLSDir() (string, error) {
	return paths.TLSDir()
}

func EnsureLocalCertificate(domain, dir string) (*LocalCertificate, error) {
	domain = normalizeCertificateDomain(domain)
	if domain == "" {
		return nil, errors.New("missing exposure certificate authority domain")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		var err error
		dir, err = DefaultTLSDir()
		if err != nil {
			return nil, err
		}
	}
	certPath := filepath.Join(dir, LocalCertificateFilename)
	keyPath := filepath.Join(dir, LocalCertificateKeyFilename)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		cert, err := parseLocalCertificate(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		if localCertificateIsUsableCA(cert.Cert) && localCertificateKeyMatches(cert.Cert, cert.Key) {
			cert.CertPath = certPath
			cert.KeyPath = keyPath
			return cert, nil
		}
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
	default:
		return nil, fmt.Errorf("load exposure certificate from %s: cert error=%v key error=%v", dir, certErr, keyErr)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create exposure TLS directory %s: %w", dir, err)
	}
	cert, err := generateLocalCertificateAuthority(domain)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, cert.CertPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write exposure certificate: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(cert.Key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write exposure certificate key: %w", err)
	}
	cert.CertPath = certPath
	cert.KeyPath = keyPath
	return cert, nil
}

func RemoveLocalCertificateFiles(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		var err error
		dir, err = DefaultTLSDir()
		if err != nil {
			return err
		}
	}
	var removeErr error
	for _, name := range []string{LocalCertificateFilename, LocalCertificateKeyFilename} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return removeErr
}

func GenerateServerCertificate(domain, dir string) (tls.Certificate, error) {
	domain = normalizeCertificateDomain(domain)
	if domain == "" {
		return tls.Certificate{}, errors.New("missing exposure certificate domain")
	}
	ca, err := EnsureRuntimeCertificateAuthority(domain, dir)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate exposure server certificate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate exposure server certificate serial: %w", err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 1, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create exposure server certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse generated exposure server certificate: %w", err)
	}
	certPEM := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		ca.CertPEM...,
	)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load exposure server certificate: %w", err)
	}
	pair.Leaf = leaf
	return pair, nil
}

// EnsureRuntimeCertificateAuthority loads the local CA used to mint HTTPS exposure leaves.
func EnsureRuntimeCertificateAuthority(domain, dir string) (*LocalCertificate, error) {
	domain = normalizeCertificateDomain(domain)
	if domain == "" {
		return nil, errors.New("missing exposure certificate authority domain")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		var err error
		dir, err = DefaultTLSDir()
		if err != nil {
			return nil, err
		}
	}
	certPath := filepath.Join(dir, LocalCertificateFilename)
	keyPath := filepath.Join(dir, LocalCertificateKeyFilename)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		cert, err := parseLocalCertificate(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		if localCertificateIsUsableCA(cert.Cert) && localCertificateKeyMatches(cert.Cert, cert.Key) {
			cert.CertPath = certPath
			cert.KeyPath = keyPath
			return cert, nil
		}
		return nil, fmt.Errorf("%w: run sudo cleanroom dns install to refresh %s", ErrLocalCertificateRequiresInstall, certPath)
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		return EnsureLocalCertificate(domain, dir)
	default:
		return nil, fmt.Errorf("load exposure certificate from %s: cert error=%v key error=%v", dir, certErr, keyErr)
	}
}

func parseLocalCertificate(certPEM, keyPEM []byte) (*LocalCertificate, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, errors.New("exposure certificate PEM is invalid")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse exposure certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		return nil, errors.New("exposure certificate key PEM is invalid")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse exposure certificate key: %w", err)
	}
	return &LocalCertificate{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

func generateLocalCertificateAuthority(domain string) (*LocalCertificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate exposure certificate authority key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate exposure certificate authority serial: %w", err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "Cleanroom Local Exposure CA (" + domain + ")",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create exposure certificate authority: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated exposure certificate authority: %w", err)
	}
	return &LocalCertificate{
		Cert:    cert,
		Key:     key,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
	}, nil
}

func normalizeCertificateDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func localCertificateIsUsableCA(cert *x509.Certificate) bool {
	if cert == nil || !cert.IsCA {
		return false
	}
	if time.Until(cert.NotAfter) < 24*time.Hour {
		return false
	}
	return cert.KeyUsage&x509.KeyUsageCertSign != 0
}

func localCertificateKeyMatches(cert *x509.Certificate, key *rsa.PrivateKey) bool {
	if cert == nil || key == nil {
		return false
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return false
	}
	return pub.E == key.PublicKey.E && pub.N.Cmp(key.PublicKey.N) == 0
}

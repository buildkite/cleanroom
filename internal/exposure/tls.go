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
	"slices"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/paths"
)

const (
	LocalCertificateFilename    = "exposure-cert.pem"
	LocalCertificateKeyFilename = "exposure-cert.key"
)

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

func EnsureLocalCertificateWithDomains(domain, dir string, extraDomains []string) (*LocalCertificate, error) {
	domain = normalizeCertificateDomain(domain)
	if domain == "" {
		return nil, errors.New("missing exposure certificate domain")
	}
	dnsNames, err := normalizeCertificateDNSNames(domain, extraDomains)
	if err != nil {
		return nil, err
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
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
		if localCertificateMatchesDNSNames(cert.Cert, dnsNames) {
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
	cert, err := generateLocalCertificate(domain, dnsNames)
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

func GenerateServerCertificateWithDomains(domain, dir string, extraDomains []string) (tls.Certificate, error) {
	cert, err := EnsureLocalCertificateWithDomains(domain, dir, extraDomains)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(cert.Key)})
	pair, err := tls.X509KeyPair(cert.CertPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load exposure certificate: %w", err)
	}
	pair.Leaf = cert.Cert
	return pair, nil
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

func generateLocalCertificate(domain string, dnsNames []string) (*LocalCertificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate exposure certificate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate exposure certificate serial: %w", err)
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: domain,
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create exposure certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated exposure certificate: %w", err)
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

func NormalizeAdditionalCertificateDomains(domain string, extraDomains []string) ([]string, error) {
	domain = normalizeCertificateDomain(domain)
	if domain == "" {
		return nil, errors.New("missing exposure certificate domain")
	}
	out := make([]string, 0, len(extraDomains))
	for _, name := range extraDomains {
		name = normalizeCertificateDomain(name)
		if name == "" {
			continue
		}
		if err := validateAdditionalCertificateDomain(domain, name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func normalizeCertificateDNSNames(domain string, extraDomains []string) ([]string, error) {
	normalizedExtra, err := NormalizeAdditionalCertificateDomains(domain, extraDomains)
	if err != nil {
		return nil, err
	}
	dnsNames := append([]string{domain, "*." + domain}, normalizedExtra...)
	slices.Sort(dnsNames)
	return slices.Compact(dnsNames), nil
}

func validateAdditionalCertificateDomain(domain, name string) error {
	switch {
	case name == domain:
	case name == "*."+domain:
	case strings.HasPrefix(name, "*."):
		suffix := strings.TrimPrefix(name, "*.")
		if suffix == "" {
			return fmt.Errorf("additional exposure certificate domain %q must include a wildcard suffix", name)
		}
		if !strings.EqualFold(suffix, domain) && !strings.HasSuffix(suffix, "."+domain) {
			return fmt.Errorf("additional exposure certificate domain %q must be under %q", name, domain)
		}
		if err := validateDNSName(suffix); err != nil {
			return fmt.Errorf("additional exposure certificate domain %q: %w", name, err)
		}
		if strings.Contains(suffix, "*") {
			return fmt.Errorf("additional exposure certificate domain %q must use a single leading wildcard", name)
		}
	default:
		if strings.Contains(name, "*") {
			return fmt.Errorf("additional exposure certificate domain %q must use a single leading wildcard", name)
		}
		if !strings.EqualFold(name, domain) && !strings.HasSuffix(name, "."+domain) {
			return fmt.Errorf("additional exposure certificate domain %q must be under %q", name, domain)
		}
		if err := validateDNSName(name); err != nil {
			return fmt.Errorf("additional exposure certificate domain %q: %w", name, err)
		}
	}
	return nil
}

func localCertificateMatchesDNSNames(cert *x509.Certificate, dnsNames []string) bool {
	if cert == nil || cert.IsCA {
		return false
	}
	if time.Until(cert.NotAfter) < 24*time.Hour {
		return false
	}
	actual := append([]string(nil), cert.DNSNames...)
	for i := range actual {
		actual[i] = normalizeCertificateDomain(actual[i])
	}
	slices.Sort(actual)
	actual = slices.Compact(actual)
	return slices.Equal(actual, dnsNames)
}

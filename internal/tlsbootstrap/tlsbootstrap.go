// Package tlsbootstrap generates CA and leaf certificates for cleanroom mTLS.
package tlsbootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCommonName = "cleanroom-ca"
	validity     = 365 * 24 * time.Hour
)

// KeyPair holds PEM-encoded certificate and private key material.
type KeyPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GenerateCA creates a self-signed ECDSA P-256 CA certificate.
func GenerateCA() (*KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	return &KeyPair{
		CertPEM: encodeCertPEM(certDER),
		KeyPEM:  encodeKeyPEM(key),
	}, nil
}

// IssueCert creates a leaf certificate signed by the given CA.
// The name is used as the CommonName. SANs may include DNS names and IP
// addresses (parsed automatically).
func IssueCert(caCertPEM, caKeyPEM []byte, name string, sans []string) (*KeyPair, error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	dnsNames, ips := classifySANs(sans)
	if len(dnsNames) == 0 && len(ips) == 0 {
		if ip := net.ParseIP(name); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, name)
		}
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now,
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}

	return &KeyPair{
		CertPEM: encodeCertPEM(certDER),
		KeyPEM:  encodeKeyPEM(key),
	}, nil
}

// Init generates a full set of TLS material (CA, server cert, client cert) and
// writes it to dir. Returns an error if the CA already exists unless force is true.
func Init(dir string, force bool) error {
	caPath := filepath.Join(dir, "ca.pem")
	if !force {
		if _, err := os.Stat(caPath); err == nil {
			return fmt.Errorf("CA already exists at %s (use --force to overwrite)", caPath)
		}
	}

	ca, err := GenerateCA()
	if err != nil {
		return err
	}

	server, err := IssueCert(ca.CertPEM, ca.KeyPEM, "cleanroom-server", []string{"localhost", "127.0.0.1", "::1"})
	if err != nil {
		return fmt.Errorf("issue server certificate: %w", err)
	}

	client, err := IssueCert(ca.CertPEM, ca.KeyPEM, "cleanroom-client", nil)
	if err != nil {
		return fmt.Errorf("issue client certificate: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create TLS directory: %w", err)
	}

	files := map[string][]byte{
		"ca.pem":     ca.CertPEM,
		"ca.key":     ca.KeyPEM,
		"server.pem": server.CertPEM,
		"server.key": server.KeyPEM,
		"client.pem": client.CertPEM,
		"client.key": client.KeyPEM,
	}
	for name, data := range files {
		perm := os.FileMode(0o644)
		if filepath.Ext(name) == ".key" {
			perm = 0o600
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, perm); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	return nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decode CA key PEM")
	}
	rawKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	return cert, rawKey, nil
}

func classifySANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, s)
		}
	}
	return dnsNames, ips
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

func encodeCertPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKeyPEM(key *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

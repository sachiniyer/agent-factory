package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The TLS material for the daemon's TCP surface (#1592 Phase 3 PR1, §1.2).
// TLS is mandatory on the TCP listener — the bearer token must never ride the
// wire in the clear. This file only produces and resolves the cert/key; it is
// DARK: no listener uses it yet (Phase 3 PR3 binds the TLS TCP listener).
//
// Default (zero-config): a self-generated ECDSA P-256 self-signed cert stored
// in the af home. The client pins its SHA-256 fingerprint (TOFU), so a
// hostname/SAN mismatch is irrelevant — connecting by IP or through an
// ssh -L tunnel Just Works. Override: user-provided PEM cert/key (Let's
// Encrypt, corporate CA), verified against system roots (no pin needed).

const (
	// daemonTLSCertFileName / daemonTLSKeyFileName are the self-signed PEM
	// cert and key in the af home. The key is 0600; the cert is public.
	daemonTLSCertFileName = "daemon-tls.crt"
	daemonTLSKeyFileName  = "daemon-tls.key"
	// selfSignedValidity is how long a self-generated cert is valid. It is
	// long-lived because it is pinned by fingerprint, not chained to a CA:
	// rotation would only change the pin the client already trusts.
	selfSignedValidity = 10 * 365 * 24 * time.Hour
)

// TLSMaterial is the resolved cert/key the daemon's TCP listener will serve
// (Phase 3 PR3). SelfSigned distinguishes the pinned self-signed default from
// a user-provided CA cert (which the client verifies against system roots).
type TLSMaterial struct {
	// CertPath / KeyPath are the PEM files on disk.
	CertPath string
	KeyPath  string
	// SelfSigned is true when the daemon generated the cert (client must pin
	// the fingerprint); false when the user supplied tls_cert/tls_key.
	SelfSigned bool
}

// ResolveTLSMaterial returns the TLS cert/key for the daemon's TCP surface.
//
// When both userCert and userKey are set it uses them verbatim (the override
// path) and does not self-generate. When both are empty it loads — or, on
// first use, generates — a self-signed ECDSA cert under dir. Setting exactly
// one of the pair is a configuration error, since a cert without its key (or
// vice versa) cannot serve TLS.
func ResolveTLSMaterial(dir, userCert, userKey string) (TLSMaterial, error) {
	switch {
	case userCert != "" && userKey != "":
		if _, err := os.Stat(userCert); err != nil {
			return TLSMaterial{}, fmt.Errorf("tls_cert %s: %w", userCert, err)
		}
		if _, err := os.Stat(userKey); err != nil {
			return TLSMaterial{}, fmt.Errorf("tls_key %s: %w", userKey, err)
		}
		return TLSMaterial{CertPath: userCert, KeyPath: userKey, SelfSigned: false}, nil
	case userCert != "" || userKey != "":
		return TLSMaterial{}, fmt.Errorf(
			"tls_cert and tls_key must be set together (got tls_cert=%q, tls_key=%q)", userCert, userKey)
	default:
		certPath := filepath.Join(dir, daemonTLSCertFileName)
		keyPath := filepath.Join(dir, daemonTLSKeyFileName)
		if err := ensureSelfSignedCert(certPath, keyPath); err != nil {
			return TLSMaterial{}, err
		}
		return TLSMaterial{CertPath: certPath, KeyPath: keyPath, SelfSigned: true}, nil
	}
}

// ensureSelfSignedCert loads the self-signed cert/key at the given paths,
// generating and persisting a fresh pair only when either file is missing. It
// is idempotent — once generated the pair is reused, so the pinned
// fingerprint stays stable across daemon restarts.
func ensureSelfSignedCert(certPath, keyPath string) error {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		return nil
	}
	if certErr != nil && !os.IsNotExist(certErr) {
		return fmt.Errorf("stat tls cert: %w", certErr)
	}
	if keyErr != nil && !os.IsNotExist(keyErr) {
		return fmt.Errorf("stat tls key: %w", keyErr)
	}
	return generateSelfSignedCert(certPath, keyPath)
}

// generateSelfSignedCert writes a fresh self-signed ECDSA P-256 cert/key pair
// to the given paths. SANs cover loopback (v4 + v6) and localhost plus the
// machine hostname, so a same-host or tunneled connection matches even before
// the pin makes SAN validation moot. The key is written 0600; the cert 0644.
func generateSelfSignedCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate tls key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate tls serial: %w", err)
	}

	dnsNames := []string{"localhost"}
	if host, hErr := os.Hostname(); hErr == nil && host != "" && host != "localhost" {
		dnsNames = append(dnsNames, host)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "agent-factory daemon"},
		NotBefore:             now.Add(-time.Hour), // tolerate small clock skew
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create tls cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal tls key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return fmt.Errorf("create tls directory: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write tls cert: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write tls key: %w", err)
	}
	// Force 0600 on the key regardless of umask (WriteFile masks the mode).
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return fmt.Errorf("chmod tls key: %w", err)
	}
	return nil
}

// CertFingerprint returns the SHA-256 fingerprint of the leaf certificate in
// the PEM file at certPath, formatted "sha256:<lowercase-hex>". This is the
// value the client TOFU-pins (§1.2) and that `af token show` prints. It is
// computed over the certificate DER, so it is stable as long as the cert file
// is (self-signed certs are generated once and reused).
func CertFingerprint(certPath string) (string, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("no PEM certificate found in %s", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

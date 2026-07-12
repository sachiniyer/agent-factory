package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseCertFile decodes and parses the leaf certificate from a PEM file.
func parseCertFile(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block in cert file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert
}

func TestResolveTLSMaterialSelfSignedValid(t *testing.T) {
	dir := t.TempDir()

	material, err := ResolveTLSMaterial(dir, "", "")
	if err != nil {
		t.Fatalf("ResolveTLSMaterial: %v", err)
	}
	if !material.SelfSigned {
		t.Fatal("expected SelfSigned=true for the zero-config path")
	}
	if material.CertPath != filepath.Join(dir, daemonTLSCertFileName) {
		t.Fatalf("unexpected cert path %q", material.CertPath)
	}

	// The generated pair loads as a usable TLS keypair.
	if _, err := tls.LoadX509KeyPair(material.CertPath, material.KeyPath); err != nil {
		t.Fatalf("generated cert/key is not a valid keypair: %v", err)
	}

	cert := parseCertFile(t, material.CertPath)

	// SANs cover loopback + localhost.
	hasLocalhost := false
	for _, name := range cert.DNSNames {
		if name == "localhost" {
			hasLocalhost = true
		}
	}
	if !hasLocalhost {
		t.Fatalf("cert DNSNames %v missing localhost", cert.DNSNames)
	}
	hasLoopback := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Fatalf("cert IPAddresses %v missing 127.0.0.1", cert.IPAddresses)
	}

	// Long-lived validity window (pinned, not CA-chained).
	if remaining := time.Until(cert.NotAfter); remaining < 365*24*time.Hour {
		t.Fatalf("cert expires too soon: NotAfter in %v", remaining)
	}

	// The key file is 0600; the public cert is not required to be.
	info, err := os.Stat(material.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file perms = %o, want 0600", perm)
	}
}

func TestResolveTLSMaterialFingerprintStable(t *testing.T) {
	dir := t.TempDir()

	first, err := ResolveTLSMaterial(dir, "", "")
	if err != nil {
		t.Fatalf("ResolveTLSMaterial (first): %v", err)
	}
	fp1, err := CertFingerprint(first.CertPath)
	if err != nil {
		t.Fatalf("CertFingerprint (first): %v", err)
	}

	// A second resolve must reuse the persisted cert (not regenerate), so the
	// pinned fingerprint is stable across daemon restarts.
	second, err := ResolveTLSMaterial(dir, "", "")
	if err != nil {
		t.Fatalf("ResolveTLSMaterial (second): %v", err)
	}
	fp2, err := CertFingerprint(second.CertPath)
	if err != nil {
		t.Fatalf("CertFingerprint (second): %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across resolves: %q != %q", fp1, fp2)
	}
	if !strings.HasPrefix(fp1, "sha256:") || len(fp1) != len("sha256:")+64 {
		t.Fatalf("unexpected fingerprint format: %q", fp1)
	}
}

func TestResolveTLSMaterialUserOverride(t *testing.T) {
	dir := t.TempDir()

	// A self-signed pair we can reuse as a stand-in "user-provided" cert.
	certPath := filepath.Join(dir, "user.crt")
	keyPath := filepath.Join(dir, "user.key")
	if err := generateSelfSignedCert(certPath, keyPath); err != nil {
		t.Fatalf("generate user cert: %v", err)
	}

	material, err := ResolveTLSMaterial(dir, certPath, keyPath)
	if err != nil {
		t.Fatalf("ResolveTLSMaterial override: %v", err)
	}
	if material.SelfSigned {
		t.Fatal("expected SelfSigned=false when a user cert is provided")
	}
	if material.CertPath != certPath || material.KeyPath != keyPath {
		t.Fatalf("override not honored: %+v", material)
	}

	// The override must be used verbatim: the self-signed default file must
	// not have been generated.
	if _, err := os.Stat(filepath.Join(dir, daemonTLSCertFileName)); !os.IsNotExist(err) {
		t.Fatalf("self-signed cert was generated despite an override (err=%v)", err)
	}
}

func TestResolveTLSMaterialOverrideErrors(t *testing.T) {
	dir := t.TempDir()

	// Only one of the pair set: a configuration error.
	if _, err := ResolveTLSMaterial(dir, "/x/cert.pem", ""); err == nil {
		t.Fatal("want error when only tls_cert is set")
	}
	if _, err := ResolveTLSMaterial(dir, "", "/x/key.pem"); err == nil {
		t.Fatal("want error when only tls_key is set")
	}

	// Both set but the files do not exist.
	if _, err := ResolveTLSMaterial(dir, "/x/cert.pem", "/x/key.pem"); err == nil {
		t.Fatal("want error when the override files do not exist")
	}
}

func TestCertFingerprintNonCert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notacert.pem")
	if err := os.WriteFile(path, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CertFingerprint(path); err == nil {
		t.Fatal("want error fingerprinting a non-certificate file")
	}
}

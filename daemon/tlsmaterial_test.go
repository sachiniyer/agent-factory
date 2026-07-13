package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestEnsureSelfSignedCertConcurrent is the #1683 regression guard. Daemon
// startup and `af token show` both call ensureSelfSignedCert, and before the
// fix the check-then-generate was unserialized and the cert/key were written
// with two separate, non-atomic os.WriteFile calls. Racing callers could
// interleave those writes and leave a cert and key from different keypairs on
// disk, which tls.LoadX509KeyPair rejects — the daemon's TLS TCP listener then
// fails to start. Here N goroutines hammer ensureSelfSignedCert on a fresh dir
// across many iterations; the persisted pair must ALWAYS load as a matching
// keypair. On the pre-fix code this fails on a large fraction of iterations;
// with the file lock + atomic writes it never does.
func TestEnsureSelfSignedCertConcurrent(t *testing.T) {
	const iterations = 100
	const goroutines = 12

	for iter := 0; iter < iterations; iter++ {
		dir := t.TempDir()
		certPath := filepath.Join(dir, daemonTLSCertFileName)
		keyPath := filepath.Join(dir, daemonTLSKeyFileName)

		var wg sync.WaitGroup
		var start sync.WaitGroup
		start.Add(1)
		errs := make([]error, goroutines)
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start.Wait() // release all goroutines together to widen the race window
				errs[idx] = ensureSelfSignedCert(certPath, keyPath)
			}(i)
		}
		start.Done()
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("iter %d goroutine %d: ensureSelfSignedCert: %v", iter, i, err)
			}
		}

		// The whole point: the persisted cert and key must be a matching pair.
		if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
			t.Fatalf("iter %d: persisted cert/key is a mismatched pair: %v", iter, err)
		}
	}
}

// TestEnsureSelfSignedCertTokenShowStartupRace models the specific #1683 race:
// `af token show` (which resolves the material to print a fingerprint) and
// daemon startup calling ResolveTLSMaterial at the same moment on the same af
// home. Both must observe a usable keypair, and both must agree on the pinned
// fingerprint — otherwise `af token show` prints a fingerprint for a cert the
// daemon cannot serve.
func TestEnsureSelfSignedCertTokenShowStartupRace(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		dir := t.TempDir()

		var wg sync.WaitGroup
		var start sync.WaitGroup
		start.Add(1)
		results := make([]TLSMaterial, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				start.Wait()
				results[idx], errs[idx] = ResolveTLSMaterial(dir, "", "")
			}(i)
		}
		start.Done()
		wg.Wait()

		for i := 0; i < 2; i++ {
			if errs[i] != nil {
				t.Fatalf("iter %d caller %d: ResolveTLSMaterial: %v", iter, i, errs[i])
			}
			if _, err := tls.LoadX509KeyPair(results[i].CertPath, results[i].KeyPath); err != nil {
				t.Fatalf("iter %d caller %d: mismatched cert/key pair: %v", iter, i, err)
			}
		}

		// Both callers must pin the same fingerprint (single generated cert).
		fp0, err := CertFingerprint(results[0].CertPath)
		if err != nil {
			t.Fatalf("iter %d: fingerprint caller 0: %v", iter, err)
		}
		fp1, err := CertFingerprint(results[1].CertPath)
		if err != nil {
			t.Fatalf("iter %d: fingerprint caller 1: %v", iter, err)
		}
		if fp0 != fp1 {
			t.Fatalf("iter %d: callers disagree on pinned fingerprint: %q != %q", iter, fp0, fp1)
		}
	}
}

// TestEnsureSelfSignedCertRegeneratesMismatchedPair covers the residual #1683
// window the concurrent-race fix alone left open: two files that both EXIST but
// come from different keypairs. This arises from a crash between the key and
// cert renames when a cert already existed (new key + old cert), manual
// tampering, or a stale mismatched pair predating the fix. An existence-only
// check blesses it, and the daemon's tls.LoadX509KeyPair then fails so the
// TLS TCP listener won't start. ensureSelfSignedCert must instead detect the
// mismatch and regenerate a fresh matching pair.
func TestEnsureSelfSignedCertRegeneratesMismatchedPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, daemonTLSCertFileName)
	keyPath := filepath.Join(dir, daemonTLSKeyFileName)

	// Seed a valid pair, capture its cert, then overwrite the KEY with a key
	// from a different, independently generated pair. Now cert and key both
	// exist but do not pair.
	if err := generateSelfSignedCert(certPath, keyPath); err != nil {
		t.Fatalf("seed initial pair: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("sanity: seeded pair should load: %v", err)
	}
	origFingerprint, err := CertFingerprint(certPath)
	if err != nil {
		t.Fatalf("fingerprint seeded cert: %v", err)
	}

	otherCert := filepath.Join(dir, "other.crt")
	otherKey := filepath.Join(dir, "other.key")
	if err := generateSelfSignedCert(otherCert, otherKey); err != nil {
		t.Fatalf("generate foreign pair: %v", err)
	}
	foreignKey, err := os.ReadFile(otherKey)
	if err != nil {
		t.Fatalf("read foreign key: %v", err)
	}
	if err := os.WriteFile(keyPath, foreignKey, 0o600); err != nil {
		t.Fatalf("overwrite key with foreign key: %v", err)
	}
	// Precondition: the on-disk pair is now mismatched.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		t.Fatal("precondition failed: seeded pair is not mismatched")
	}

	// ensureSelfSignedCert must NOT bless the mismatch — it must regenerate.
	if err := ensureSelfSignedCert(certPath, keyPath); err != nil {
		t.Fatalf("ensureSelfSignedCert on mismatched pair: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("cert/key still mismatched after ensureSelfSignedCert: %v", err)
	}
	// It regenerated (fresh pair), so the fingerprint changed from the seed.
	newFingerprint, err := CertFingerprint(certPath)
	if err != nil {
		t.Fatalf("fingerprint regenerated cert: %v", err)
	}
	if newFingerprint == origFingerprint {
		t.Fatal("expected a fresh cert after regeneration, got the original fingerprint")
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

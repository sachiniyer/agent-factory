package agentproto

import (
	"crypto/tls"
	"strings"
	"testing"
)

// TestNormalizeFingerprint_AcceptsFormsRejectsGarbage proves the pin accepts the
// sha256:-prefixed form the daemon/agent-server print, plain hex, and
// colon-separated hex, and rejects wrong-length / non-hex input up front (before
// any dial).
func TestNormalizeFingerprint_AcceptsFormsRejectsGarbage(t *testing.T) {
	const bare = "aa11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddee"
	forms := []string{bare, "sha256:" + bare, strings.ToUpper(bare), "SHA256:" + bare}
	for _, f := range forms {
		got, err := NormalizeFingerprint(f)
		if err != nil {
			t.Fatalf("NormalizeFingerprint(%q): %v", f, err)
		}
		if got != bare {
			t.Fatalf("NormalizeFingerprint(%q) = %q, want %q", f, got, bare)
		}
	}
	// Colon-separated hex (openssl's default form) normalizes to the same bare hex.
	colon := "aa:11:bb:22:cc:33:dd:44:ee:55:ff:66:00:77:88:99:00:11:22:33:44:55:66:77:88:99:00:aa:bb:cc:dd:ee"
	if got, err := NormalizeFingerprint(colon); err != nil || got != bare {
		t.Fatalf("NormalizeFingerprint(colon) = (%q,%v), want (%q,nil)", got, err, bare)
	}
	for _, f := range []string{"", "abcd", bare + "ff", "zz11" + bare[4:]} {
		if _, err := NormalizeFingerprint(f); err == nil {
			t.Fatalf("NormalizeFingerprint(%q): want error, got nil", f)
		}
	}
}

// TestPinnedTLSConfig_PinReplacesDefaultVerify proves a fingerprint pin takes over
// verification (InsecureSkipVerify with a VerifyPeerCertificate callback) while an
// empty fingerprint leaves standard system-root verification in place.
func TestPinnedTLSConfig_PinReplacesDefaultVerify(t *testing.T) {
	const bare = "aa11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddee"
	pinned, err := PinnedTLSConfig("sha256:" + bare)
	if err != nil {
		t.Fatalf("PinnedTLSConfig(pin): %v", err)
	}
	if !pinned.InsecureSkipVerify || pinned.VerifyPeerCertificate == nil {
		t.Fatalf("pinned config must hand verification to the callback, got %+v", pinned)
	}
	if pinned.MinVersion != tls.VersionTLS12 {
		t.Fatalf("pinned config must floor at TLS 1.2, got %x", pinned.MinVersion)
	}
	// The callback is the sole arbiter: no cert ⇒ handshake fails.
	if err := pinned.VerifyPeerCertificate(nil, nil); err == nil {
		t.Fatal("pin callback must reject an empty cert chain")
	}

	ca, err := PinnedTLSConfig("")
	if err != nil {
		t.Fatalf("PinnedTLSConfig(\"\"): %v", err)
	}
	if ca.InsecureSkipVerify || ca.VerifyPeerCertificate != nil {
		t.Fatalf("no-pin config must keep standard system-root verification, got %+v", ca)
	}

	if _, err := PinnedTLSConfig("not-a-fingerprint"); err == nil {
		t.Fatal("PinnedTLSConfig must reject a malformed fingerprint")
	}
}

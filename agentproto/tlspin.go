package agentproto

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
)

// TLS pinning is part of the wire-auth material (§4.4, alongside the bearer
// token above): a client reaching an `af` TLS listener — a remote daemon
// (apiclient, #1592 Phase 3) or a remote agent-server (session, #1592 Phase 4) —
// either pins the listener's self-signed cert by SHA-256 fingerprint (TOFU) or,
// when no fingerprint is given, verifies a real CA cert against the system trust
// store. Both clients dial the same kind of listener with the same auth, so the
// pin logic lives here as ONE source of truth rather than duplicated per client.
//
// TLS is MANDATORY on those listeners (the bearer token would ride the wire in
// the clear otherwise) and verification is NEVER skipped: the pin REPLACES the
// default CA-chain + hostname check with a stronger exact-fingerprint match, so
// connecting by IP or through a tunnel works despite the self-signed cert's SAN
// while a substituted cert is refused.

// PinnedTLSConfig builds the client TLS config for a remote `af` TLS listener.
// With a fingerprint it pins the leaf cert's SHA-256 (TOFU): the default CA-chain
// + hostname check is REPLACED — not skipped — by a VerifyPeerCertificate
// callback that requires an exact fingerprint match, so an IP/tunnel connection
// to the pinned identity works while a substituted cert fails the handshake.
// Without a fingerprint the listener must present a cert that chains to a system
// root (a real CA cert), verified normally.
//
// Pinning a bare SHA-256 (a hash, not the cert) is only expressible in Go by
// taking over verification with a callback; the standard-library idiom for that
// pairs the callback with InsecureSkipVerify (below) so the callback is the SOLE
// arbiter. The connection is never actually unverified — an unmatched cert fails
// the handshake.
func PinnedTLSConfig(fingerprint string) (*tls.Config, error) {
	if fingerprint == "" {
		// CA-cert path: standard system-root verification, TLS 1.2 floor.
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	want, err := NormalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The default CA-chain + hostname check is handed off to the pin below —
		// NOT skipped. It cannot validate a self-signed cert, and its SAN check
		// would wrongly reject a legitimate IP/tunnel connection to the pinned
		// identity; VerifyPeerCertificate enforces the stronger fingerprint match
		// on every handshake instead, so an unmatched cert still fails.
		InsecureSkipVerify: true,
		// VerifyPeerCertificate is the sole arbiter: it fails the handshake unless
		// the leaf's SHA-256 exactly matches the pin. rawCerts[0] is the leaf DER
		// — the same bytes CertFingerprint hashes — so the comparison is
		// apples-to-apples. Hostname/SAN is deliberately not checked (§1.2).
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("remote host presented no TLS certificate")
			}
			got := CertFingerprint(rawCerts[0])
			if got != want {
				return fmt.Errorf("TLS fingerprint mismatch: pinned sha256:%s but host presented sha256:%s "+
					"(wrong host, or the cert was regenerated — re-check the fingerprint on the host)", want, got)
			}
			return nil
		},
	}, nil
}

// NormalizeFingerprint canonicalizes a user-supplied pin to bare lowercase hex.
// It accepts the `sha256:<hex>` form the daemon/agent-server print, plain hex,
// and colon- or space-separated hex, and requires exactly 32 bytes (a SHA-256).
func NormalizeFingerprint(fingerprint string) (string, error) {
	s := strings.TrimSpace(fingerprint)
	s = strings.TrimPrefix(strings.ToLower(s), "sha256:")
	s = strings.NewReplacer(":", "", " ", "").Replace(s)
	if len(s) != sha256.Size*2 {
		return "", fmt.Errorf("invalid TLS fingerprint %q: expected a SHA-256 hex string (optionally sha256:-prefixed)", fingerprint)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("invalid TLS fingerprint %q: not hexadecimal: %w", fingerprint, err)
	}
	return s, nil
}

// CertFingerprint returns the lowercase-hex SHA-256 of a DER-encoded certificate,
// matching the daemon's `sha256:<hex>` banner form minus the prefix (the daemon
// hashes the same DER bytes: cs.PeerCertificates[0].Raw == the leaf DER).
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

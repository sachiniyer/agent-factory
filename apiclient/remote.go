package apiclient

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The REMOTE transport for the daemon HTTP/WS API (#1592 Phase 3 PR4, §1.6). A
// Client built with NewRemote dials a daemon over TCP+TLS instead of the local
// unix socket, so a TUI/CLI on machine A can drive a daemon on machine B over the
// PR3 TLS listener. Everything downstream of the transport — the {data,error}
// envelope decode, the agentproto WS codec, ui/termpane, the attach proxy — is
// byte-identical to the local path; only the dialer, the base URL, and the
// bearer token differ.
//
// TLS is MANDATORY (the token would ride the wire in the clear otherwise), and
// verification is NEVER skipped: the client either pins the daemon's self-signed
// cert by SHA-256 fingerprint (TOFU, §1.2) or, when no fingerprint is given,
// verifies a real CA cert against the system trust store.

// remoteDialTimeout bounds the TCP connect to a remote daemon. Unlike the local
// unix socket (a quarter second — it is either there or not), a remote dial
// crosses a real network, so it is given a longer, network-appropriate budget.
const remoteDialTimeout = 10 * time.Second

// NewRemote returns a Client that dials the remote daemon at daemonURL over
// TCP+TLS, threading `token` on every REST call and WS handshake. daemonURL is a
// TLS base URL — `wss://host:port` or `https://host:port` (the two are
// equivalent; both select TLS and only the authority is used). When
// `fingerprint` is non-empty the client pins the daemon's leaf cert to that
// SHA-256 value (the self-signed default, printed by `af token show`); when it is
// empty the daemon's cert is verified against the system trust store (a CA cert).
func NewRemote(daemonURL, token, fingerprint string) (*Client, error) {
	httpBase, wsBase, err := parseDaemonURL(daemonURL)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := pinnedTLSConfig(fingerprint)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteDialTimeout}
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext:     dialer.DialContext,
				TLSClientConfig: tlsCfg,
			},
		},
		token:    token,
		httpBase: httpBase,
		wsBase:   wsBase,
	}, nil
}

// parseDaemonURL validates a remote daemon base URL and derives the REST
// (https://host:port) and WS (wss://host:port) authorities from it. It accepts
// the TLS schemes `wss` and `https` interchangeably (a daemon serves REST and WS
// on the same TLS listener) and rejects plaintext `ws`/`http` — there is no
// clear-text TCP mode (§4). Only the scheme and authority are used; any path or
// query on the URL is ignored.
func parseDaemonURL(daemonURL string) (httpBase, wsBase string, err error) {
	u, err := url.Parse(strings.TrimSpace(daemonURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid --daemon-url %q: %w", daemonURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
	case "ws", "http":
		return "", "", fmt.Errorf("--daemon-url %q uses a plaintext scheme; the remote daemon is TLS-only, use wss:// or https://", daemonURL)
	default:
		return "", "", fmt.Errorf("--daemon-url %q must be a wss:// or https:// URL", daemonURL)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("--daemon-url %q has no host:port", daemonURL)
	}
	return "https://" + u.Host, "wss://" + u.Host, nil
}

// pinnedTLSConfig builds the client TLS config for a remote daemon. With a
// fingerprint it pins the leaf cert's SHA-256 (TOFU): default chain/hostname
// verification is replaced — NOT disabled — by a VerifyConnection callback that
// requires an exact fingerprint match, so connecting by IP or through a tunnel
// works despite the self-signed cert's SAN, while a substituted cert is refused.
// Without a fingerprint the daemon must present a cert that chains to a system
// root (a real CA cert), verified normally.
func pinnedTLSConfig(fingerprint string) (*tls.Config, error) {
	if fingerprint == "" {
		// CA-cert path: standard system-root verification, TLS 1.2 floor.
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	want, err := normalizeFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// InsecureSkipVerify turns off the default chain+hostname check ONLY so
		// the VerifyConnection pin below can stand in for it. This is not
		// "skip verification": every handshake must still pass the exact
		// fingerprint match or the connection is refused. Hostname/SAN is
		// deliberately not checked so IP/tunnel connections to the pinned cert
		// succeed (§1.2).
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("remote daemon presented no TLS certificate")
			}
			got := certFingerprint(cs.PeerCertificates[0].Raw)
			if got != want {
				return fmt.Errorf("TLS fingerprint mismatch: pinned sha256:%s but daemon presented sha256:%s "+
					"(wrong daemon, or the cert was regenerated — re-check `af token show` on the daemon host)", want, got)
			}
			return nil
		},
	}, nil
}

// normalizeFingerprint canonicalizes a user-supplied pin to bare lowercase hex.
// It accepts the `sha256:<hex>` form `af token show` prints, plain hex, and
// colon- or space-separated hex, and requires exactly 32 bytes (a SHA-256).
func normalizeFingerprint(fingerprint string) (string, error) {
	s := strings.TrimSpace(fingerprint)
	s = strings.TrimPrefix(strings.ToLower(s), "sha256:")
	s = strings.NewReplacer(":", "", " ", "").Replace(s)
	if len(s) != sha256.Size*2 {
		return "", fmt.Errorf("invalid --tls-fingerprint %q: expected a SHA-256 hex string (optionally sha256:-prefixed)", fingerprint)
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("invalid --tls-fingerprint %q: not hexadecimal: %w", fingerprint, err)
	}
	return s, nil
}

// certFingerprint returns the lowercase-hex SHA-256 of a DER-encoded certificate,
// matching daemon.CertFingerprint's `sha256:<hex>` form minus the prefix (the
// daemon hashes the same DER bytes: cs.PeerCertificates[0].Raw == the leaf DER).
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

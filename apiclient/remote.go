package apiclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
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
	// The TLS pin (self-signed TOFU fingerprint or system-root CA verification)
	// is shared wire-auth material, single-sourced in agentproto so this remote
	// daemon client and the remote agent-server client (#1592 Phase 4) pin
	// identically.
	tlsCfg, err := agentproto.PinnedTLSConfig(fingerprint)
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

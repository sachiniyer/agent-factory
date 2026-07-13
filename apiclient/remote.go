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

// Timeouts bounding a remote round-trip. Unlike the local unix socket (a quarter
// second — it is either there or not), a remote target crosses a real network and
// a half-open or wedged daemon must surface as a clear timeout error instead of
// hanging forever (#1730). Each is chosen to bound a STALL, not to race a
// legitimately slow call:
//
//   - remoteDialTimeout — the TCP connect.
//   - remoteTLSHandshakeTimeout — the TLS handshake. Its absence was the #1730
//     hang: a peer that accepts the TCP connection but never completes the
//     handshake blocked every REST and WS call indefinitely. 10s matches Go's
//     http.DefaultTransport default.
//   - remoteResponseHeaderTimeout — the wait for the server's response headers
//     after the request is written. This bounds a daemon that completes TLS but
//     never answers (REST) or never returns the WS 101 (a stalled handshake on
//     the WS path). It is stream-SAFE: it caps only the time to the response
//     headers, never a live stream's subsequent reads, so a long-lived PTY
//     subscription is untouched.
//   - remoteRequestTimeout — the overall deadline on a single REST call()
//     round-trip (applied via the request context, remote-only). WS/stream dials
//     are deliberately EXEMPT from this overall cap — they carry only the two
//     transport bounds above so a long-lived stream is never severed mid-flight.
//
// They are vars (not consts) only so a test can shrink them to prove the bound
// fires without waiting the full budget — the same pattern attach.go's
// attachDrainTimeout/attachWriteTimeout use.
var (
	remoteDialTimeout           = 10 * time.Second
	remoteTLSHandshakeTimeout   = 10 * time.Second
	remoteResponseHeaderTimeout = 30 * time.Second
	remoteRequestTimeout        = 60 * time.Second
)

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
			// No http.Client.Timeout: it would fire on the whole request lifetime,
			// which for the WS handshake path means the established stream itself —
			// coder/websocket explicitly warns against it. The remote round-trip is
			// bounded instead by the transport's handshake/response-header timeouts
			// (stream-safe) plus a per-call context deadline on the REST path only.
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSClientConfig:       tlsCfg,
				TLSHandshakeTimeout:   remoteTLSHandshakeTimeout,
				ResponseHeaderTimeout: remoteResponseHeaderTimeout,
			},
		},
		token:          token,
		httpBase:       httpBase,
		wsBase:         wsBase,
		requestTimeout: remoteRequestTimeout,
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

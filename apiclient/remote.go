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
// a half-open or wedged daemon must surface as a clear error instead of hanging
// forever (#1730). There are exactly two connection-level bounds plus ONE overall
// REST deadline — no per-layer split, so there is a single number to reason about:
//
//   - remoteDialTimeout — the TCP connect.
//   - remoteTLSHandshakeTimeout — the TLS handshake. Its absence was the #1730
//     hang: a peer that accepts the TCP connection but never completes the
//     handshake blocked every REST and WS call indefinitely. 10s matches Go's
//     http.DefaultTransport default. These two catch the common unreachable /
//     wedged-at-connect cases FAST, for both REST and WS.
//   - remoteRequestTimeout — the single overall deadline on one REST call()
//     round-trip (applied via the request context, remote-only). It is the sole
//     REST wait-bound: a daemon that completes TLS but then never sends a response
//     surfaces here instead of hanging. It is deliberately GENEROUS, not snappy,
//     because a mutating RPC runs SYNCHRONOUSLY inside the request — a remote
//     docker/ssh CreateSession (#1592 Phase 4) can spend minutes provisioning
//     (image pull, ssh spin-up) before the daemon writes response headers, and a
//     tight deadline would sever a create that is actually succeeding server-side
//     and orphan it. The value clears realistic provisioning; a truly wedged
//     connection is still bounded (just not instantly), and the fast-fail cases
//     are already covered by dial + handshake above.
//
// Notably there is NO transport-level ResponseHeaderTimeout and NO
// http.Client.Timeout: a shared ResponseHeaderTimeout would fire mid-provision on
// a slow synchronous create (Greptile #1734), and http.Client.Timeout would
// additionally kill an established WS stream (coder/websocket warns against it).
// WS/stream dials are EXEMPT from the overall deadline entirely — they carry only
// the dial + handshake bounds, so a long-lived PTY subscription is never severed.
//
// They are vars (not consts) only so a test can shrink them to prove the bound
// fires without waiting the full budget — the same pattern attach.go's
// attachDrainTimeout/attachWriteTimeout use.
var (
	remoteDialTimeout         = 10 * time.Second
	remoteTLSHandshakeTimeout = 10 * time.Second
	remoteRequestTimeout      = 5 * time.Minute
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
			// No http.Client.Timeout: it fires on the whole request lifetime, which
			// for the WS handshake path is the established stream itself
			// (coder/websocket warns against it). The overall REST bound is applied
			// per-call as a request-context deadline instead (call(), remote-only),
			// so WS/stream dials stay exempt.
			Transport: &http.Transport{
				DialContext:         dialer.DialContext,
				TLSClientConfig:     tlsCfg,
				TLSHandshakeTimeout: remoteTLSHandshakeTimeout,
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

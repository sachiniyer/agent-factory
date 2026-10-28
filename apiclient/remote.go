package apiclient

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The REMOTE transport for the daemon HTTP/WS API (#1592 Phase 3 PR4, §1.6;
// HTTP-only rework 2026-07-14). A Client built with NewRemote dials a daemon
// over plain HTTP/WS instead of the local unix socket, so a TUI/CLI on machine A
// can drive a daemon on machine B over the PR3 listener. Everything downstream
// of the transport — the {data,error} envelope decode, the agentproto WS codec,
// ui/termpane, the attach proxy — is byte-identical to the local path; only the
// dialer, the base URL, and the bearer token differ.
//
// The daemon itself serves plaintext HTTP and terminates no TLS. A client may
// still reach it over TLS when something in front of it does: `tailscale serve`,
// a reverse proxy, a cloud load balancer. `https://` and `wss://` targets are
// therefore accepted (W9.5) and verified against the SYSTEM root CAs — the
// transport keeps Go's default TLS config with a TLS 1.2 floor and there is, by
// design, no flag that skips verification. A plain `http://` target still sends
// the optional bearer token in the clear, so it belongs on a private network
// (Tailscale/VPN/SSH tunnel) or behind such a TLS front.

// Timeouts bounding a remote round-trip. Unlike the local unix socket (a quarter
// second — it is either there or not), a remote target crosses a real network and
// a half-open or wedged daemon must surface as a clear error instead of hanging
// forever (#1730). There is one connection-level bound plus ONE overall REST
// deadline — a single number to reason about at each layer:
//
//   - remoteDialTimeout — the TCP connect. Catches the common unreachable /
//     wedged-at-connect case FAST, for both REST and WS.
//   - remoteWSHandshakeTimeout — bounds the WS UPGRADE handshake (the HTTP
//     101 exchange after the TCP connect) on a remote DialStream. Plain HTTP has
//     no TLS handshake to time out, so without this a peer that accepts the TCP
//     connection but never answers the upgrade would hang the attach path (which
//     dials with context.Background()) forever — the plaintext analogue of the
//     #1730 half-open handshake. It bounds ONLY the handshake: coder/websocket's
//     Dial context does not govern the established stream, so a long-lived PTY
//     subscription is never severed by it.
//   - remoteRequestTimeout — the single overall deadline on one REST call()
//     round-trip (applied via the request context, remote-only). It is the sole
//     REST wait-bound: a daemon that accepts the connection but then never sends a
//     response surfaces here instead of hanging. It is deliberately GENEROUS, not
//     snappy, because a mutating RPC runs SYNCHRONOUSLY inside the request — a
//     remote docker/ssh CreateSession (#1592 Phase 4) can spend minutes
//     provisioning (image pull, ssh spin-up) before the daemon writes response
//     headers, and a tight deadline would sever a create that is actually
//     succeeding server-side and orphan it. The value clears realistic
//     provisioning; a truly wedged connection is still bounded (just not
//     instantly), and the fast-fail case is already covered by the dial above.
//
// Notably there is NO SHARED transport-level ResponseHeaderTimeout and NO
// http.Client.Timeout: a ResponseHeaderTimeout on the REST transport would fire
// mid-provision on a slow synchronous create (Greptile #1734), and
// http.Client.Timeout would additionally kill an established WS stream
// (coder/websocket warns against it). DialStream clones the remote transport and
// puts ResponseHeaderTimeout only on that one WS handshake. WS/stream dials are
// EXEMPT from the overall REST deadline entirely, so a long-lived PTY
// subscription is never severed.
//
// They are vars (not consts) only so a test can shrink them to prove the bound
// fires without waiting the full budget — the same pattern attach.go's
// attachDrainTimeout/attachWriteTimeout use.
var (
	remoteDialTimeout        = 10 * time.Second
	remoteWSHandshakeTimeout = 10 * time.Second
	remoteRequestTimeout     = 5 * time.Minute
)

// NewRemote returns a Client that dials the remote daemon at daemonURL,
// threading `token` on every REST call and WS handshake. daemonURL is a base URL:
// `http://host:port` / `ws://host:port` (plaintext, equivalent) or
// `https://host:port` / `wss://host:port` (TLS, equivalent, verified against the
// system CAs). Only the scheme and authority are used.
func NewRemote(daemonURL, token string) (*Client, error) {
	if socket, ok, err := parseUnixDaemonURL(daemonURL); err != nil {
		return nil, err
	} else if ok {
		return newRemoteUnix(socket, token), nil
	}
	httpBase, wsBase, err := parseDaemonURL(daemonURL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteDialTimeout}
	// TLSClientConfig is set only to pin a floor; RootCAs stays nil so the system
	// pool verifies the peer, and InsecureSkipVerify is never set.
	transport := &http.Transport{
		DialContext:     dialer.DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{
		httpClient: &http.Client{
			// No http.Client.Timeout: it fires on the whole request lifetime, which
			// for the WS handshake path is the established stream itself
			// (coder/websocket warns against it). The overall REST bound is applied
			// per-call as a request-context deadline instead (call(), remote-only),
			// so WS/stream dials stay exempt.
			Transport: transport,
		},
		remoteTransport: transport,
		token:           token,
		httpBase:        httpBase,
		wsBase:          wsBase,
		requestTimeout:  remoteRequestTimeout,
	}, nil
}

// parseDaemonURL validates a remote daemon base URL and derives the REST
// (http[s]://host:port) and WS (ws[s]://host:port) authorities from it. The
// plaintext schemes `http`/`ws` are interchangeable, as are the TLS schemes
// `https`/`wss` (a daemon serves REST and WS on the same listener, so a TLS front
// does too). Only the scheme family and authority are used; any path or query on
// the URL is ignored.
//
// The host check tests u.Hostname(), not u.Host: net/url parses `http://:8443`
// into a NON-empty Host (":8443") with an empty Hostname, so a u.Host check
// admits a hostless URL and defers the failure to an opaque `dial tcp :8443:
// connect: connection refused` (#1784). That form is what an unset variable
// expands to (`AF_DAEMON_URL="http://${DAEMON_HOST}:8443"`), so it is the case
// most worth naming precisely.
func parseDaemonURL(daemonURL string) (httpBase, wsBase string, err error) {
	u, err := url.Parse(strings.TrimSpace(daemonURL))
	if err != nil {
		return "", "", fmt.Errorf("invalid --daemon-url %q: %w", daemonURL, err)
	}
	secure := false
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	case "https", "wss":
		secure = true
	default:
		return "", "", fmt.Errorf("--daemon-url %q must be an http://, ws://, https:// or wss:// URL", daemonURL)
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("invalid --daemon-url %q: missing host; use http://HOST:PORT "+
			"(if this URL came from a variable, it likely expanded empty)", daemonURL)
	}
	if secure {
		return "https://" + u.Host, "wss://" + u.Host, nil
	}
	return "http://" + u.Host, "ws://" + u.Host, nil
}

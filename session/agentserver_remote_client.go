package session

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The remoteAgentClient half of the remote agent-server transport: the
// endpoint value the runtimes hand down, the client construction that turns
// it into a credential-bearing HTTP/WS transport, and the URL split it shares.
// The methods on the client live with remoteAgentServer in
// agentserver_remote.go.

// AgentServerEndpoint is the runtime handle that points an Instance at a remote
// agent-server (#1592 Phase 4 PR2): the authed URL of the `af agent-server` in the
// sandbox plus the auth material to reach it. A nil endpoint ⇒ the local
// in-process runtime (the default, unchanged). Phase 4 PR3 generalizes this into a
// Runtime that PROVISIONS the sandbox and fills these in; PR2 only consumes them.
type AgentServerEndpoint struct {
	// URL is the agent-server's plain-HTTP base URL — `http://host:port` or
	// `ws://host:port` (equivalent; both select the same plaintext transport,
	// only the authority is used). A wss://host:port is rejected — the
	// agent-server is HTTP-only. When SocketPath is set the URL carries an
	// unroutable placeholder authority instead: every dial goes to the socket
	// and a code path that bypassed the unix dialer fails name resolution
	// before a byte is written.
	URL string
	// Token is the bearer credential presented on every REST call and WS handshake.
	Token string
	// SocketPath, when set, is the unix socket the endpoint is reached through:
	// the local end of af's own `ssh -L <socket>:<remote>` forward (#7495).
	// The remote agent-server is still a TCP loopback listener on its host —
	// the socket is only the tunnel's LOCAL end.
	SocketPath string
	// VerifyPeer, when set, runs on every freshly dialed unix connection
	// BEFORE any request bytes are written; a non-nil result closes the
	// connection unused. The sandbox tunnel fills it with the proof that the
	// socket's acceptor is af's own ssh child's process group — so nothing
	// credential-bearing is ever sent to an endpoint that is not provably
	// af's own forward.
	VerifyPeer func(conn *net.UnixConn) error
}

// remoteAgentClient is the transport to ONE `af agent-server`: a token-bearing
// plain-HTTP client for the /v1/agent/* control REST and a matching WS dialer for
// the /v1/sessions/{title}/stream data plane. It is the session-package analogue
// of apiclient.Client (which session cannot import — the cycle), sharing the
// pieces that define the contract through the agentproto/apiproto leaves.
type remoteAgentClient struct {
	httpClient *http.Client
	transport  *http.Transport
	httpBase   string // http://host:port
	wsBase     string // ws://host:port
	token      string
	title      string // the sandbox's single-workspace session title (the stream path id)
}

// newRemoteAgentClient builds the transport for ep, validating the URL up front
// (no dial). It mirrors apiclient.NewRemote: the transport is plain HTTP/WS (no
// TLS — the sandbox is reached over a private hop) and the token rides every
// request.
func newRemoteAgentClient(ep AgentServerEndpoint, title string) (*remoteAgentClient, error) {
	if title == "" {
		return nil, fmt.Errorf("remote agent-server requires a workspace title")
	}
	httpBase, wsBase, err := splitHTTPBaseURL(ep.URL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteAgentDialTimeout}
	dial := dialer.DialContext
	if ep.SocketPath != "" {
		// The unix forward: REST, the WS stream, and the loopback relays all
		// ride this transport, so this one dial point covers every
		// credential-bearing connection. VerifyPeer runs while the request is
		// still unwritten — a refusal is a tunnel failure, never a malformed
		// or misattributed response (#7495).
		socketPath := ep.SocketPath
		verify := ep.VerifyPeer
		dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, "unix", socketPath)
			if err != nil {
				return nil, err
			}
			if verify != nil {
				uc, ok := conn.(*net.UnixConn)
				if !ok {
					_ = conn.Close()
					return nil, fmt.Errorf("remote agent-server: tunnel endpoint %q did not yield a unix connection", socketPath)
				}
				if verr := verify(uc); verr != nil {
					_ = conn.Close()
					return nil, fmt.Errorf("remote agent-server: %w", verr)
				}
			}
			return conn, nil
		}
	}
	transport := &http.Transport{DialContext: dial}
	return &remoteAgentClient{
		httpClient: &http.Client{
			Transport: transport,
		},
		transport: transport,
		httpBase:  httpBase,
		wsBase:    wsBase,
		token:     ep.Token,
		title:     title,
	}, nil
}

// splitHTTPBaseURL validates an agent-server base URL and derives its REST
// (http://host:port) and WS (ws://host:port) authorities. It accepts the
// plaintext schemes http/ws interchangeably (the agent-server serves REST and WS
// on one HTTP listener) and rejects the TLS schemes wss/https — the agent-server
// is HTTP-only (af terminates no TLS; the sandbox is reached over a private hop).
// Only scheme+authority are used. It mirrors apiclient's parseDaemonURL but with
// agent-server-appropriate error text — including its u.Hostname() host check,
// which (unlike u.Host) rejects the hostless `http://:8443` form instead of
// deferring it to an opaque dial error (#1784).
func splitHTTPBaseURL(raw string) (httpBase, wsBase string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid agent-server URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
	case "https", "wss":
		return "", "", fmt.Errorf("agent-server URL %q uses a TLS scheme, but the agent-server is HTTP-only; use http:// or ws://", raw)
	default:
		return "", "", fmt.Errorf("agent-server URL %q must be an http:// or ws:// URL", raw)
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("invalid agent-server URL %q: missing host; use http://HOST:PORT", raw)
	}
	return "http://" + u.Host, "ws://" + u.Host, nil
}

// Package apiclient is a typed Go client for the daemon-hosted HTTP/JSON API
// (#1029) that the daemon serves on its `daemon-http.sock` Unix socket. It is
// the read-side twin of the gob `net/rpc` control client in daemon/: it dials
// the SAME daemon core over a DIFFERENT transport and, by decoding the shared
// `{data,error}` envelope back into the SAME request/response structs the RPC
// client uses, it returns byte-identical results. This is the seam #1592 Phase 2
// grows the client API on — HTTP today, WebSocket streaming later — without the
// TUI or CLI ever touching the wire shape.
//
// Phase 2 PR2 scope: this client exposes only the READ-ONLY Snapshot path and
// its first consumer is the non-spawning `af sessions list`/`get` read
// (api/sessions.go). Every write/control call stays on net/rpc; the disk
// fallback is unchanged. The envelope is NOT redefined here — the client decodes
// the exact bytes daemon/httpserver.go writes via apiproto.WriteEnvelope, which
// is what guarantees parity.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// TransportError wraps a failure to REACH the daemon HTTP socket — a refused
// dial, a missing socket file, a read error mid-response — as opposed to an
// application error the daemon returned inside the envelope. The two are
// categorically different: an envelope error is a real, deterministic failure
// (session not found, repo invalid) that will recur, while a transport error on
// a just-spawned daemon is a transient bind race — the daemon binds its HTTP
// socket microseconds AFTER the control socket EnsureDaemon waits for (see the
// daemon boot order), so a call fired in that window sees connection-refused.
// Callers distinguish the two with IsTransportError so they can retry a bind
// race without ever masking a real daemon error by retrying it.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// IsTransportError reports whether err (or anything it wraps) is a TransportError
// — a failure to reach the daemon socket rather than an error the daemon
// returned. Envelope/application errors are never TransportErrors, so a caller
// retrying on this signal can never spin on a deterministic failure.
func IsTransportError(err error) bool {
	var te *TransportError
	return errors.As(err, &te)
}

// dialTimeout bounds how long the client waits to connect to the daemon HTTP
// socket. It mirrors the net/rpc control client's dial timeout so the two read
// paths fail fast identically when no daemon is listening: an absent or stale
// socket refuses immediately, and a live daemon is local, so a quarter second is
// ample. Deliberately no overall request timeout — like the net/rpc client's
// blocking Call, the read waits for the daemon's in-memory snapshot rather than
// racing an arbitrary deadline.
const dialTimeout = 250 * time.Millisecond

// localHTTPBase is a syntactic placeholder host for the LOCAL unix-socket path.
// The Unix-socket dialer ignores it (the socket path is the real address), but
// net/http requires a valid URL, so every local request targets
// http://af/v1/<Method>. A remote Client (NewRemote) carries the real
// https://host:port authority instead.
const localHTTPBase = "http://af"

// localWSBase is the placeholder WS authority for the LOCAL unix socket: the
// http.Client's transport dials the socket regardless of host, so ws://unix is
// purely syntactic. A remote Client carries the real wss://host:port authority.
const localWSBase = "ws://unix"

// Client dials the daemon's HTTP/JSON API and calls its /v1/* routes. By default
// it dials the local unix socket (New / NewWithSocket) — the transport ignores
// the URL host and connects to the fixed socket path, so a Client is bound to one
// daemon home. NewRemote instead dials a REMOTE daemon over TCP+TLS and threads a
// bearer token on every call (#1592 Phase 3 PR4); httpBase/wsBase then carry the
// real https://host:port / wss://host:port authority. A zero Client is not usable.
type Client struct {
	httpClient *http.Client
	// token is the bearer credential threaded on every REST call (Authorization
	// header) and WS dial (header + ?access_token=) for a REMOTE target. It is
	// empty for the local unix socket, whose peer is trusted (0600 perms are the
	// auth, #1029) — so the local path sends no Authorization header, unchanged.
	token string
	// httpBase is the REST scheme+authority: localHTTPBase ("http://af") for the
	// unix socket, "https://host:port" for a remote daemon.
	httpBase string
	// wsBase is the WS scheme+authority: localWSBase ("ws://unix") for the unix
	// socket, "wss://host:port" for a remote daemon.
	wsBase string
	// requestTimeout is the SINGLE overall deadline applied to each REST call()
	// round-trip via the request context. It is set only for a REMOTE target
	// (NewRemote), so a daemon that completes TLS but then never responds surfaces
	// as a timeout instead of hanging forever (#1730) — the sole REST wait-bound,
	// deliberately generous so a synchronous mutating RPC (a remote docker/ssh
	// CreateSession) is not severed mid-provision. It is 0 for the local unix
	// socket, which keeps its blocking-read semantics (the socket is either there
	// or not, bounded by dialTimeout, and the in-memory snapshot returns promptly).
	// WS stream dials never consult this field — they are bounded only by the
	// transport's dial + handshake timeouts, so a long-lived stream is never
	// severed by an overall deadline.
	requestTimeout time.Duration
}

// New returns a Client dialing the daemon HTTP socket resolved from the current
// config dir (AGENT_FACTORY_HOME), the same path the daemon binds. It does not
// dial or spawn anything — a daemon need not be running yet; the first call
// surfaces an unreachable socket as a transport error the caller collapses to
// the disk-fallback signal.
func New() (*Client, error) {
	socketPath, err := daemon.DaemonHTTPSocketPath()
	if err != nil {
		return nil, err
	}
	return NewWithSocket(socketPath), nil
}

// NewWithSocket returns a Client dialing an explicit HTTP socket path. It is the
// injection seam for tests, which bind a real Unix socket at a temp path and
// point the client at it without a full daemon.
func NewWithSocket(socketPath string) *Client {
	dialer := &net.Dialer{Timeout: dialTimeout}
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				// Ignore the URL host/port entirely and dial the Unix socket.
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socketPath)
				},
			},
		},
		httpBase: localHTTPBase,
		wsBase:   localWSBase,
	}
}

// call POSTs req as JSON to /v1/<method>, decodes the shared {data,error}
// envelope, and unmarshals the envelope's data member into resp. It is the
// single client-side twin of daemon/httpserver.go's rpcHandler: request struct
// in, response struct out, the envelope in between. A populated envelope error —
// the same string the net/rpc handler would return, since both call the same
// controlServer method — is surfaced as a Go error; a transport failure (no
// daemon listening) surfaces as the raw net/http error. resp may be nil for a
// caller that only needs success/failure.
func (c *Client) call(method string, req any, resp any) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("apiclient: marshal request: %w", err)
	}

	// A remote target bounds the whole round-trip with a single overall deadline so
	// a wedged daemon (TLS up, never responds) times out instead of hanging
	// (#1730); the deadline is generous so a slow synchronous create is not
	// severed. The local socket leaves requestTimeout zero and blocks on the
	// in-memory snapshot as before.
	ctx := context.Background()
	if c.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.httpBase+"/v1/"+method, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("apiclient: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// A remote target authenticates with a bearer token on every REST call; the
	// local unix socket carries no token (trusted transport) and this is a no-op.
	c.setAuth(httpReq.Header)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// The round-trip never reached a daemon handler — refused dial, missing
		// socket, etc. Tag it so a caller can tell a bind race from a real error.
		return &TransportError{Err: err}
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return &TransportError{Err: fmt.Errorf("apiclient: read response body: %w", err)}
	}

	// Decode into a RawMessage-backed envelope so the typed response is decoded
	// in a second pass — the daemon's data member is the RPC response struct,
	// and keeping it raw until the error branch is checked avoids interpreting a
	// failure body's null data.
	var env struct {
		Data  json.RawMessage         `json:"data"`
		Error *apiproto.EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("apiclient: malformed response envelope: %w", err)
	}
	if env.Error != nil {
		// Surface the daemon's message verbatim — byte-identical to what the
		// net/rpc client would carry, since both transports wrap the same
		// controlServer error.
		return fmt.Errorf("%s", env.Error.Message)
	}
	if resp != nil {
		if err := json.Unmarshal(env.Data, resp); err != nil {
			return fmt.Errorf("apiclient: malformed response data: %w", err)
		}
	}
	return nil
}

// setAuth adds the `Authorization: Bearer <token>` header when this Client
// targets a remote daemon. It is a no-op for the local unix socket (empty token,
// trusted transport), so the local REST path is byte-identical to before Phase 3.
// The header name/scheme are agentproto's, shared with the daemon's extractor so
// the wire contract is single-sourced.
func (c *Client) setAuth(h http.Header) {
	if c.token != "" {
		h.Set(agentproto.AuthHeader, agentproto.BearerScheme+c.token)
	}
}

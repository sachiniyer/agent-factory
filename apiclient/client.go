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
	"sync/atomic"
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

// clientVersion holds this binary's version for the agentproto.ClientVersionHeader
// every call() sends. It is atomic because SetClientVersion runs once at startup
// while calls fire from arbitrary goroutines (the TUI issues RPCs off the event
// loop), and a plain package var would race a parallel test's swap — the same
// hazard app.killInstanceCmd documents when it captures its seam.
//
// The zero state is the unknown-version fallback rather than an empty header:
// the daemon's decoder keys off the header's PRESENCE, so a client that never
// resolved its version still gets forward-compatible decoding. The value is
// diagnostics only.
var clientVersion atomic.Value // string

// unknownClientVersion is what an af client reports when SetClientVersion was
// never called — a test binary or a library consumer, never a released `af`.
const unknownClientVersion = "unknown"

// SetClientVersion stamps the version this client reports to the daemon. It is
// called once from NewRootCommand, which is the ONLY point every entrypoint
// (TUI, CLI, and `af --daemon`) passes through — app.Version is set inside the
// TUI's RunE and so would miss every CLI subcommand.
func SetClientVersion(v string) {
	if v != "" {
		clientVersion.Store(v)
	}
}

func clientVersionOrUnknown() string {
	if v, ok := clientVersion.Load().(string); ok && v != "" {
		return v
	}
	return unknownClientVersion
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
// http://host:port authority instead.
const localHTTPBase = "http://af"

// localWSBase is the placeholder WS authority for the LOCAL unix socket: the
// http.Client's transport dials the socket regardless of host, so ws://unix is
// purely syntactic. A remote Client carries the real ws://host:port authority.
const localWSBase = "ws://unix"

// Client dials the daemon's HTTP/JSON API and calls its /v1/* routes. By default
// it dials the local unix socket (New / NewWithSocket) — the transport ignores
// the URL host and connects to the fixed socket path, so a Client is bound to one
// daemon home. NewRemote instead dials a REMOTE daemon over plain HTTP/WS and threads a
// bearer token on every call (#1592 Phase 3 PR4); httpBase/wsBase then carry the
// real http://host:port / ws://host:port authority. A zero Client is not usable.
type Client struct {
	httpClient *http.Client
	// token is the bearer credential threaded on every REST call (Authorization
	// header) and WS dial (header + ?access_token=) for a REMOTE target. It is
	// empty for the local unix socket, whose peer is trusted (0600 perms are the
	// auth, #1029) — so the local path sends no Authorization header, unchanged.
	token string
	// httpBase is the REST scheme+authority: localHTTPBase ("http://af") for the
	// unix socket, "http://host:port" for a remote daemon.
	httpBase string
	// wsBase is the WS scheme+authority: localWSBase ("ws://unix") for the unix
	// socket, "ws://host:port" for a remote daemon.
	wsBase string
	// requestTimeout is the SINGLE overall deadline applied to each REST call()
	// round-trip via the request context. It is set only for a REMOTE target
	// (NewRemote), so a daemon that accepts the connection but then never responds surfaces
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
	return c.callCtx(context.Background(), method, req, resp)
}

// callCtx is call bounded by ctx. It exists because the LOCAL unix socket
// deliberately carries no overall requestTimeout (see the field's doc): most
// local RPCs block on an in-memory snapshot and return at once, and a local
// CreateSession legitimately runs long while it provisions a worktree and waits
// for agent readiness, so a blanket deadline would sever real work.
//
// That leaves a caller with no way to bound a call that MUST NOT wedge the UI.
// Nothing else bounds it: the http.Client sets no Timeout (only a 250ms
// dialTimeout on the DIAL), and the daemon's http.Server sets only
// ReadHeaderTimeout — so once the daemon accepts the connection the read is
// unbounded, and a daemon wedged in a handler (e.g. blocked acquiring a
// session's op lock behind a Lost-recovery) hangs the caller FOREVER. The TUI's
// kill path is the one that must never do that: its in-flight OpKilling fence is
// cleared only by the reply, so a lost reply strands the row in `Deleting` with
// no error and no way out. Passing a ctx here cancels the in-flight request for
// real, so the caller's goroutine unwinds instead of leaking.
func (c *Client) callCtx(ctx context.Context, method string, req any, resp any) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("apiclient: marshal request: %w", err)
	}

	// A remote target bounds the whole round-trip with a single overall deadline so
	// a wedged daemon (connected, never responds) times out instead of hanging
	// (#1730); the deadline is generous so a slow synchronous create is not
	// severed. It composes with any caller-supplied ctx — whichever fires first
	// wins. The local socket leaves requestTimeout zero and blocks on the
	// in-memory snapshot as before, unless the caller bounds it explicitly.
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
	// Identify this request as machine-generated by an af client so the daemon
	// tolerates request fields it has never heard of. The daemon is upgraded
	// independently of its clients (#960), so a newer client legitimately sends
	// additive fields an older daemon lacks — `tab_id` on PreviewRequest (#1779)
	// is what turned that skew into a hard 400. See agentproto.ClientVersionHeader
	// for why the discriminator is the header rather than a blanket-lenient
	// decoder (#1264's typo guard must survive for hand-authored requests).
	httpReq.Header.Set(agentproto.ClientVersionHeader, clientVersionOrUnknown())
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
		// controlServer error — except where the message is provably a version
		// skew, which is unactionable in its raw form.
		return interpretEnvelopeError(env.Error.Message, env.Error.Code)
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

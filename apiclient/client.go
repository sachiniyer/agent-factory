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

// httpBaseURL is a syntactic placeholder host. The Unix-socket dialer ignores it
// (the socket path is the real address), but net/http requires a valid URL, so
// every request targets http://af/v1/<Method>.
const httpBaseURL = "http://af"

// Client dials the daemon's HTTP/JSON Unix socket and calls its /v1/* routes.
// It holds a net/http.Client whose transport dials the fixed socket path, so a
// Client is bound to one daemon home. Construct it with New (resolves the socket
// from the config dir) or NewWithSocket (explicit path, for tests). A zero
// Client is not usable.
type Client struct {
	httpClient *http.Client
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

	httpResp, err := c.httpClient.Post(httpBaseURL+"/v1/"+method, "application/json", bytes.NewReader(reqBody))
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

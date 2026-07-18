package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon-hosted HTTP/JSON server (#1029 PR 4) makes HTTP just another thin
// client of the same daemon core the net/rpc control socket already exposes
// (#960). It runs INSIDE the daemon, holds the *Manager directly through a
// controlServer, and every route is a uniform 1:1 mirror of an RPC:
//
//	POST /v1/<Method>   JSON body = the RPC request struct
//	                    response  = the shared {data,error} envelope wrapping
//	                                the RPC response struct
//	GET  /v1/health     liveness alias for Ping
//
// Each route decodes the request body into the EXACT SAME request struct the
// net/rpc handler uses and calls the SAME (*controlServer) method — there is no
// forked logic, so HTTP and RPC can never drift by construction. The response is
// encoded through apiproto.WriteEnvelope, the identical primitive the CLI's
// --json path uses, so the two surfaces are byte-for-byte consistent.
//
// Transport is a dedicated Unix socket (daemon-http.sock) with 0600 perms —
// filesystem permissions are the authentication; there is no TCP port and no
// token (#1029 locked decisions).

// maxHTTPBodyBytes caps a request body so a client cannot exhaust daemon memory.
// 16 MiB comfortably fits any realistic prompt or task payload. It is enforced
// via http.MaxBytesReader, which REJECTS an oversize body (→ 413) rather than
// silently truncating it. var, not const, so tests can shrink it.
var maxHTTPBodyBytes int64 = 16 << 20

// httpReadHeaderTimeout bounds how long the server waits for request headers,
// closing the Slowloris hold-open window (gosec G112). The socket is local and
// 0600, but a defensive default costs nothing.
const httpReadHeaderTimeout = 10 * time.Second

// DaemonHTTPSocketPath returns the path of the daemon's HTTP/JSON Unix socket.
//
// Length-checked at resolution for the same reason as DaemonSocketPath: see
// there, and #1940. This name is the longest of the daemon's sockets, so it is
// the one that overruns first.
func DaemonHTTPSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, daemonHTTPSocketFileName)
	if err := sockpath.Check("daemon HTTP socket", path); err != nil {
		return "", err
	}
	return path, nil
}

// startHTTPServer binds the HTTP/JSON server on its own Unix socket and serves
// it in the background, returning a cleanup function that shuts the server down
// and unlinks the socket. It shares the daemon's live *Manager (via a
// controlServer built the same way startControlServer builds its own), so both
// transports dispatch through one core. The Shutdown RPC is deliberately NOT
// wired here (shutdownCh nil): HTTP mirrors only the client-facing surface, not
// daemon lifecycle control.
func startHTTPServer(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor) (func() error, error) {
	socketPath, err := DaemonHTTPSocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}

	cs := &controlServer{manager: manager, scheduler: scheduler, watchers: watchers}
	// One mux, shared by both listeners, so the REST/RPC/WS handler graph is
	// single-sourced and the two transports can never drift (§1.1).
	mux := newHTTPMux(cs)
	srv := &http.Server{
		// The unix socket is trusted transport (0600 perms are the auth, #1029),
		// so it passes a NIL gate: no token enforcement (#1592 Phase 3 PR2,
		// §1.4). The TCP listener below passes a real gate over the same mux.
		// CORS is config-driven (§1.5): empty allow-list ⇒ no ACAO emitted.
		Handler:           withAuth(mux, nil, manager.cfg.CORSAllowedOrigins),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		// Serve returns ErrServerClosed on a clean Close/Shutdown; anything else
		// is worth logging (the listener died unexpectedly).
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon HTTP server stopped: %v", err)
		}
	}()

	// Plain-HTTP TCP listener (#1592 Phase 3 PR3, §1.1; HTTP-only since
	// 2026-07-14) — the daemon's bundled web UI + HTTP/WS surface. ON BY
	// DEFAULT: listen_addr defaults to loopback ("127.0.0.1:8443"), so a daemon
	// with no config serves the browser client on localhost. An explicit
	// `listen_addr = ""` is the opt-out that skips this block entirely
	// (pure-unix daemon). It serves the same mux behind a token-enforcing gate.
	// A bind failure — including the loopback default losing a port race with
	// another daemon — is logged but NEVER fatal: the unix socket and control
	// plane every local client depends on must not regress because a web port
	// could not open, so the daemon keeps running with the web server skipped.
	closeTCP := func() error { return nil }
	if manager.cfg.ListenAddr != "" {
		// The daemon's web listener exempts loopback peers from the token only when
		// the listener is LOOPBACK-BOUND (a same-machine browser gets the
		// unix-socket's local trust, #1696), honors require_token=false to drop the
		// token for all peers on a trusted network, and withdraws the loopback
		// exemption on require_loopback_token=true. Crucially, a NETWORK-bound
		// listener never exempts loopback: a same-host reverse proxy connects from
		// 127.0.0.1, so exempting it would bypass the token (webListenerPolicy).
		policy := webListenerPolicy(manager.cfg)
		// Defense in depth for #2090. RunDaemon already refuses to start in this
		// configuration, so in production this is unreachable — it is kept because
		// it makes the bind site safe on its OWN terms rather than by trusting its
		// caller: any future entry point that reaches startHTTPServer without
		// RunDaemon's gate still cannot open an unauthenticated network listener.
		// This used to be a log warning that then bound the listener anyway, which
		// is the #2090 exposure exactly: the operator on the reporting box had the
		// warning in their log and served the control plane regardless.
		//
		// Skipping the listener (rather than failing startup) matches this
		// function's contract that the web server is never allowed to take the
		// unix control plane down with it.
		if config.ListenerServesUnauthenticatedNetwork(manager.cfg.ListenAddr, manager.cfg.RequireToken) {
			log.ErrorLog.Printf("refusing to serve the daemon web API on %q: it is reachable from the network but require_token is false, which would expose the full control API (including DeliverPrompt) unauthenticated. Set require_token = true, or bind listen_addr to 127.0.0.1. The web server is disabled for this run; the local unix control plane is unaffected.", manager.cfg.ListenAddr)
		} else if closer, info, err := startTCPListener(mux, manager.cfg, policy, withWebShell); err != nil {
			log.WarningLog.Printf("failed to start daemon HTTP TCP listener on %q: %v", manager.cfg.ListenAddr, err)
		} else {
			closeTCP = closer
			log.InfoLog.Printf("daemon HTTP TCP listener enabled on %s (plain HTTP — terminate TLS at a proxy if needed)", info.Addr)
			log.InfoLog.Printf("  bearer token: %s", info.Token)
			switch {
			case policy.tokenDisabled:
				log.InfoLog.Printf("  all peers connect with NO token (require_token defaults to false; set require_token = true to require auth)")
			case policy.loopbackExempt:
				log.InfoLog.Printf("  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above")
			case config.IsLoopbackListenAddr(manager.cfg.ListenAddr):
				log.InfoLog.Printf("  require_loopback_token=true: every peer (loopback included) must present the token above")
			default:
				log.InfoLog.Printf("  listener is network-bound: every peer must present the token above, INCLUDING loopback-origin requests — a same-host reverse proxy is NOT exempt (front it and let the proxy pass the token, or set require_token=false only on a fully trusted network)")
			}
		}
	}

	return func() error {
		// Close stops the listener (which unlinks the Unix socket file, net's
		// default for a listener it created) and terminates active connections.
		// Deliberately no explicit os.Remove: mirrors startControlServer's
		// #718/#767 reasoning — a Remove could race a freshly bound socket.
		tcpErr := closeTCP()
		if err := srv.Close(); err != nil {
			return err
		}
		return tcpErr
	}, nil
}

// newHTTPMux builds the route table by iterating servedHTTPRoutes() — the public
// `af api` catalog (httpRoutes) plus the internal, non-cataloged routes
// (internalHTTPRoutes). Every route has a POST /v1/<Method> handler dispatching
// to the matching controlServer method; GET /v1/health is a liveness alias for
// Ping. The public catalog (#1029 PR 5) still mirrors only client-facing session
// and task ops; the internal routes (#1592 Phase 2 PR3) let the TUI drop net/rpc
// and reach ResumeFromLimit and the Pause/ResumeStatusPoll attach-coordination
// over HTTP without advertising them in `af api`. Shutdown, ReloadTasks, and
// bare Ping remain absent from both — daemon lifecycle, not a client verb.
func newHTTPMux(cs *controlServer) *http.ServeMux {
	mux := http.NewServeMux()

	for _, rt := range servedHTTPRoutes() {
		mux.HandleFunc(rt.Path, rt.handler(cs))
	}

	// WS data plane (#1592 Phase 2 PR5): the PTY stream broker + its stream-info
	// indirection, and the events-plane fan-out. These are NOT REST/RPC mirrors —
	// they upgrade to WebSockets — so they live outside servedHTTPRoutes (the `af
	// api` RPC catalog) and register here with method+path patterns (a non-GET to
	// a stream path is a 405 from the mux). The TUI consumes them for live panes,
	// attach, and events (#1592 Phase 2 PR6/PR7), and they ride the same auth/CORS
	// seam startHTTPServer wraps the mux in.
	mux.HandleFunc("GET /v1/sessions/{id}/stream", cs.streamHandler)
	mux.HandleFunc("GET /v1/sessions/{id}/stream-info", cs.streamInfoHandler)
	mux.HandleFunc("GET /v1/events", cs.eventsHandler)

	// The web-tab reverse proxy: not a JSON-envelope RPC (it relays a dev
	// server's raw HTTP/asset/WS traffic), so it is registered directly here like
	// the stream routes rather than in the httpRoutes catalog. No method filter —
	// a framed dev app issues GET/POST/WS to its own origin (this proxy path).
	//
	// {tabId} is the tab's STABLE id (#1738), not its ordinal: an ordinal-keyed
	// route silently repointed an open preview at a different dev server whenever a
	// LOWER tab was closed (#1810). There is no deprecated ordinal fallback — the
	// web client ships embedded in this same binary (web/embed.go), so there is no
	// version skew to bridge, and an ordinal that resolved would be the very
	// misroute this addresses. A stale id 404s; {rest...} is the mirrored upstream
	// path (see webTabProxyHandler).
	mux.HandleFunc(webtabPathPrefix+"{sessionId}/{tabId}/{rest...}", cs.webTabProxyHandler)

	// Catch-all: any other path is an unknown route → 404 with the envelope.
	// ServeMux routes the longest prefix match, so a real route above always
	// wins and only genuinely-unknown paths (e.g. /v1/Nope, /) land here.
	//
	// On the unix socket this 404 is the final word — the socket never serves the
	// web app (#1592 Phase 5 PR2). On the TCP listener, webShellHandler
	// (tcpserver.go) sits IN FRONT of this mux and intercepts every non-/v1 path
	// to serve the embedded SPA, so the browser sees index.html here instead of
	// this 404; only genuinely-unknown /v1/ paths still reach it there.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusNotFound, fmt.Errorf("unknown route %q", r.URL.Path))
	})

	return mux
}

// rpcHandler adapts a controlServer method — the same one net/rpc dispatches —
// into an HTTP handler. It enforces POST, decodes the JSON body into the RPC
// request struct, calls the method, and encodes the response struct in the
// shared envelope. Status mapping (kept pragmatic, documented here):
//
//	200 — success: {"data": <resp>, "error": null}
//	400 — malformed / unreadable JSON request body, or unknown request field
//	405 — wrong HTTP verb (not POST)
//	413 — request body exceeds maxHTTPBodyBytes
//	500 — the handler (validation or execution) returned an error
//
// 404 for unknown routes is handled by the mux's catch-all, not here. A rejected
// body (400 or 413) short-circuits before the manager call, so an oversize or
// malformed request is never dispatched.
func rpcHandler[Req any, Resp any](call func(Req, *Resp) error) http.HandlerFunc {
	return rpcHandlerCtx(func(_ context.Context, req Req, resp *Resp) error {
		return call(req, resp)
	})
}

// rpcHandlerCtx is rpcHandler for handlers that take the request context, so a
// long-running RPC (CreateSession's readiness wait) is cancelled when the HTTP
// client disconnects — r.Context() is done the moment the connection drops. This
// is what stops an abandoned create from leaving a pane-poll spinning on the
// daemon.
func rpcHandlerCtx[Req any, Resp any](call func(context.Context, Req, *Resp) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeHTTPError(w, r, http.StatusMethodNotAllowed,
				fmt.Errorf("method %s not allowed; use POST", r.Method))
			return
		}
		var req Req
		if err := decodeHTTPRequest(w, r, &req); err != nil {
			// An oversize body (MaxBytesReader tripped) is a distinct 413;
			// everything else is ordinary malformed input → 400.
			status := http.StatusBadRequest
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
			}
			writeHTTPError(w, r, status, err)
			return
		}
		var resp Resp
		if err := call(r.Context(), req, &resp); err != nil {
			writeHTTPError(w, r, http.StatusInternalServerError, err)
			return
		}
		writeHTTPSuccess(w, r, resp)
	}
}

// healthHandler answers GET /v1/health by calling Ping, giving a trivial
// liveness probe (curl the socket, look for {"data":{"ok":true},"error":null}).
func healthHandler(cs *controlServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, r, http.StatusMethodNotAllowed,
				fmt.Errorf("method %s not allowed; use GET", r.Method))
			return
		}
		var resp PingResponse
		_ = cs.Ping(PingRequest{}, &resp)
		writeHTTPSuccess(w, r, resp)
	}
}

// decodeHTTPRequest reads the size-capped body and unmarshals it into dst. An
// empty body is treated as a zero-value request so no-argument RPCs (ListTasks,
// an all-repo Snapshot, health) work with `curl -d ”` or no body at all.
//
// Unknown-field handling depends on WHO sent the request, because the same
// "unknown field" means opposite things to the two populations of caller:
//
//   - HAND-AUTHORED (curl, `af api`, a script): no agentproto.ClientVersionHeader.
//     An unknown key is almost certainly a typo, and dropping it silently can
//     WIDEN an RPC — a typo'd `repo_idd` leaves RepoID empty and turns a one-repo
//     Snapshot into an all-repo Snapshot. These stay STRICT, preserving #1264/#1273
//     exactly (see TestHTTP_UnknownJSONField_400).
//   - AN af CLIENT (TUI/CLI/web): carries the header. The daemon is upgraded
//     INDEPENDENTLY of its clients (#960 makes it the sole writer, and `af upgrade`
//     restarts it under live TUIs), so a newer client legitimately sends additive
//     fields this daemon has never heard of. Strict-rejecting them turns every
//     version skew into a hard failure: shipping `tab_id` on PreviewRequest (#1779)
//     made a newer TUI's 100ms preview poll 400 against any older daemon with
//     `unknown field "tab_id"`. Per the #1029 additive-envelope contract those
//     fields MUST degrade gracefully, so unknown keys are IGNORED here.
//
// The header is not trusted input and this is not an auth boundary: a hand-rolled
// request that sets it merely opts out of typo checking, which is the same deal
// every af client takes. Note this only helps daemons built from this change
// onward — an already-deployed older daemon still rejects, which is why the
// client also self-diagnoses the skew (apiclient.skewError).
//
// The cap is enforced with http.MaxBytesReader (NOT io.LimitReader): once the
// body exceeds maxHTTPBodyBytes the read fails with an *http.MaxBytesError, so an
// oversize request is REJECTED, never truncated-then-accepted. The error is
// returned wrapped (chain preserved) so the caller can errors.As it to a 413.
func decodeHTTPRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("request body exceeds %d-byte limit: %w", maxHTTPBodyBytes, err)
		}
		return fmt.Errorf("failed to read request body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if r.Header.Get(agentproto.ClientVersionHeader) == "" {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("malformed JSON request body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("malformed JSON request body: multiple JSON values")
		}
		return fmt.Errorf("malformed JSON request body: %w", err)
	}
	return nil
}

// writeHTTPSuccess encodes data in a success envelope with a 200.
func writeHTTPSuccess(w http.ResponseWriter, r *http.Request, data any) {
	writeHTTPEnvelope(w, r, http.StatusOK, apiproto.Success(data))
}

// writeHTTPError encodes err in a failure envelope with the given status. The
// envelope body is always returned on error, never a bare status.
func writeHTTPError(w http.ResponseWriter, r *http.Request, status int, err error) {
	writeHTTPEnvelope(w, r, status, apiproto.Failure(err.Error()))
}

// writeHTTPEnvelope is the single write path for both success and failure so the
// Content-Type, status, and byte-identical envelope shape stay uniform.
func writeHTTPEnvelope(w http.ResponseWriter, r *http.Request, status int, env apiproto.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := apiproto.WriteEnvelope(w, env); err != nil && !httpResponseWriteAbandoned(r, err) {
		log.WarningLog.Printf("failed to write HTTP response envelope: %v", err)
	}
}

// httpResponseWriteAbandoned reports a response write that failed after the
// client disconnected. Raw socket errors are expected only after the request
// context has been canceled; otherwise they are warnings worth investigating.
func httpResponseWriteAbandoned(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, net.ErrClosed) ||
		(r != nil && r.Context().Err() != nil &&
			(errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)))
}

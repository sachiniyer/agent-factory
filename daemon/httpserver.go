package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
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
func DaemonHTTPSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonHTTPSocketFileName), nil
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

	// TLS TCP listener (#1592 Phase 3 PR3, §1.1) — the daemon's bundled web UI +
	// HTTP/WS surface. ON BY DEFAULT: listen_addr defaults to loopback
	// ("127.0.0.1:8443"), so a daemon with no config serves the browser client on
	// localhost. An explicit `listen_addr = ""` is the opt-out that skips this
	// block entirely (pure-unix daemon). It serves the same mux behind a
	// token-enforcing gate. A bind failure — including the loopback default
	// losing a port race with another daemon — is logged but NEVER fatal: the
	// unix socket and control plane every local client depends on must not
	// regress because a web port could not open, so the daemon keeps running with
	// the web server skipped.
	closeTCP := func() error { return nil }
	if manager.cfg.ListenAddr != "" {
		// The daemon's web listener exempts loopback peers from the token (a
		// same-machine browser gets the unix-socket's local trust, #1696) and
		// honors require_token=false to drop the token for network peers too on a
		// trusted network. Both are safe relaxations of the token ONLY — TLS stays
		// mandatory in startTCPListener regardless.
		policy := tokenGatePolicy{
			tokenDisabled:  !manager.cfg.RequireToken,
			loopbackExempt: true,
		}
		if !manager.cfg.RequireToken {
			// A network-reachable, tokenless control plane is a deliberate but
			// dangerous choice: anyone who can reach listen_addr has full control.
			// Make it impossible to miss in the daemon log.
			log.WarningLog.Printf("WARNING: require_token=false — the daemon web API on %q accepts NETWORK peers with NO token; anyone who can reach it has full control. TLS still applies; this disables ONLY the bearer token. Unset require_token (or set it true) to re-enable auth.", manager.cfg.ListenAddr)
		}
		if closer, info, err := startTCPListener(mux, manager.cfg, policy); err != nil {
			log.WarningLog.Printf("failed to start daemon TLS TCP listener on %q: %v", manager.cfg.ListenAddr, err)
		} else {
			closeTCP = closer
			log.InfoLog.Printf("daemon TLS TCP listener enabled on %s (self-signed=%v)", info.Addr, info.SelfSigned)
			log.InfoLog.Printf("  cert fingerprint: %s", info.Fingerprint)
			log.InfoLog.Printf("  bearer token: %s", info.Token)
			log.InfoLog.Printf("  loopback peers (127.0.0.1/::1) connect with no token; network peers %s",
				map[bool]string{true: "must present the token above", false: "also need NO token (require_token=false)"}[manager.cfg.RequireToken])
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
		writeHTTPError(w, http.StatusNotFound, fmt.Errorf("unknown route %q", r.URL.Path))
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
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed,
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
			writeHTTPError(w, status, err)
			return
		}
		var resp Resp
		if err := call(req, &resp); err != nil {
			writeHTTPError(w, http.StatusInternalServerError, err)
			return
		}
		writeHTTPSuccess(w, resp)
	}
}

// healthHandler answers GET /v1/health by calling Ping, giving a trivial
// liveness probe (curl the socket, look for {"data":{"ok":true},"error":null}).
func healthHandler(cs *controlServer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, http.StatusMethodNotAllowed,
				fmt.Errorf("method %s not allowed; use GET", r.Method))
			return
		}
		var resp PingResponse
		_ = cs.Ping(PingRequest{}, &resp)
		writeHTTPSuccess(w, resp)
	}
}

// decodeHTTPRequest reads the size-capped body and unmarshals it into dst. An
// empty body is treated as a zero-value request so no-argument RPCs (ListTasks,
// an all-repo Snapshot, health) work with `curl -d ”` or no body at all.
// Unknown fields are rejected so typo'd request keys cannot silently fall back
// to zero-value RPC semantics such as an empty RepoID meaning all repos.
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
	dec.DisallowUnknownFields()
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
func writeHTTPSuccess(w http.ResponseWriter, data any) {
	writeHTTPEnvelope(w, http.StatusOK, apiproto.Success(data))
}

// writeHTTPError encodes err in a failure envelope with the given status. The
// envelope body is always returned on error, never a bare status.
func writeHTTPError(w http.ResponseWriter, status int, err error) {
	writeHTTPEnvelope(w, status, apiproto.Failure(err.Error()))
}

// writeHTTPEnvelope is the single write path for both success and failure so the
// Content-Type, status, and byte-identical envelope shape stay uniform.
func writeHTTPEnvelope(w http.ResponseWriter, status int, env apiproto.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := apiproto.WriteEnvelope(w, env); err != nil {
		log.WarningLog.Printf("failed to write HTTP response envelope: %v", err)
	}
}

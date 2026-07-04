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
	srv := &http.Server{
		Handler:           newHTTPMux(cs),
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		// Serve returns ErrServerClosed on a clean Close/Shutdown; anything else
		// is worth logging (the listener died unexpectedly).
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.WarningLog.Printf("daemon HTTP server stopped: %v", err)
		}
	}()

	return func() error {
		// Close stops the listener (which unlinks the Unix socket file, net's
		// default for a listener it created) and terminates active connections.
		// Deliberately no explicit os.Remove: mirrors startControlServer's
		// #718/#767 reasoning — a Remove could race a freshly bound socket.
		return srv.Close()
	}, nil
}

// newHTTPMux builds the route table. Every client-facing RPC gets a
// POST /v1/<Method> route that dispatches to the matching controlServer method;
// GET /v1/health is a liveness alias for Ping. Pure-infra RPCs (Shutdown,
// Pause/ResumeStatusPoll, ReloadTasks, and bare Ping) are intentionally absent —
// the HTTP surface mirrors only client-facing session and task ops.
func newHTTPMux(cs *controlServer) *http.ServeMux {
	mux := http.NewServeMux()

	// Sessions.
	mux.HandleFunc("/v1/CreateSession", rpcHandler(cs.CreateSession))
	mux.HandleFunc("/v1/Snapshot", rpcHandler(cs.Snapshot))
	mux.HandleFunc("/v1/KillSession", rpcHandler(cs.KillSession))
	mux.HandleFunc("/v1/ArchiveSession", rpcHandler(cs.ArchiveSession))
	mux.HandleFunc("/v1/RestoreArchived", rpcHandler(cs.RestoreArchived))
	mux.HandleFunc("/v1/SendPrompt", rpcHandler(cs.SendPrompt))
	mux.HandleFunc("/v1/DeliverPrompt", rpcHandler(cs.DeliverPrompt))
	mux.HandleFunc("/v1/CreateTab", rpcHandler(cs.CreateTab))
	mux.HandleFunc("/v1/CloseTab", rpcHandler(cs.CloseTab))
	mux.HandleFunc("/v1/SetPRInfo", rpcHandler(cs.SetPRInfo))
	mux.HandleFunc("/v1/ImportRemoteHookSessions", rpcHandler(cs.ImportRemoteHookSessions))

	// Tasks.
	mux.HandleFunc("/v1/ListTasks", rpcHandler(cs.ListTasks))
	mux.HandleFunc("/v1/AddTask", rpcHandler(cs.AddTask))
	mux.HandleFunc("/v1/UpdateTask", rpcHandler(cs.UpdateTask))
	mux.HandleFunc("/v1/RemoveTask", rpcHandler(cs.RemoveTask))
	mux.HandleFunc("/v1/TriggerTask", rpcHandler(cs.TriggerTask))

	// Liveness. GET-only alias for the Ping RPC.
	mux.HandleFunc("/v1/health", healthHandler(cs))

	// Catch-all: any other path is an unknown route → 404 with the envelope.
	// ServeMux routes the longest prefix match, so a real route above always
	// wins and only genuinely-unknown paths (e.g. /v1/Nope, /) land here.
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
//	400 — malformed / unreadable JSON request body
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
	if err := json.Unmarshal(body, dst); err != nil {
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

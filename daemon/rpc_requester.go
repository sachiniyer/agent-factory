package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

type rpcRequesterContextKey struct{}

// httpPeerPIDContextKey carries the kernel-verified pid of the process on the
// other end of a unix-socket HTTP connection, stamped by ConnContext when the
// connection is accepted (httpserver.go). It is the HTTP carrier of the same
// identity the control socket keeps in controlServer.requesterPID — the kernel
// recorded it at connect time, so nothing in the request can mint it (#5182).
type httpPeerPIDContextKey struct{}

// withHTTPRPCRequester records the authenticated HTTP principal and transport
// peer on the request context. Destructive handlers consume this value when
// they write their audit line; the ordinary net/rpc methods have no per-call
// context and therefore use the control-socket fallback in rpcRequester.
func withHTTPRPCRequester(r *http.Request) context.Context {
	principal := "operator"
	if owner, isSandbox := sandboxOwner(r.Context()); isSandbox {
		principal = fmt.Sprintf("sandbox session %q", owner)
	}
	peer := strings.TrimSpace(r.RemoteAddr)
	if peer == "" || peer == "@" {
		peer = "daemon HTTP Unix socket"
	}
	requester := fmt.Sprintf("HTTP %s peer %s", principal, peer)
	return context.WithValue(r.Context(), rpcRequesterContextKey{}, requester)
}

// rpcRequesterIsHTTP reports whether this call arrived over the daemon's HTTP
// surface rather than the owner-only control socket. It reads the SAME context
// value the audit line does, so the two can never disagree about which transport
// a request came in on.
func rpcRequesterIsHTTP(ctx context.Context) bool {
	requester, ok := ctx.Value(rpcRequesterContextKey{}).(string)
	return ok && requester != ""
}

func rpcRequester(ctx context.Context) string {
	if requester, ok := ctx.Value(rpcRequesterContextKey{}).(string); ok && requester != "" {
		return requester
	}
	return "control socket"
}

// trackTeardownRequester registers this call's kernel-verified requester
// process for the handler's duration and returns the unregister (#5182).
//
// The pid comes from the connection, never from request fields: on the
// control socket it is the SO_PEERCRED/LOCAL_PEERPID value the accept loop
// recorded into controlServer.requesterPID; over the unix-socket HTTP API it
// is the same kernel answer carried on the request context by ConnContext. A
// client-supplied pid cannot reach this code path at all, so a process cannot
// claim an exemption it is not owed (the issue's property (d)).
//
// The pid names a process SLOT, not an identity: it is resolved through
// proctree.Lookup so the registry tracks a (pid, start-stamp) instance, and a
// pid recycled between accept and here fails that match rather than exempting
// a stranger. Anything that cannot produce a verified identity — a read
// failure, an unsupported platform, a dead peer — tracks nothing, which is
// exactly the pre-#5182 behavior.
func (s *controlServer) trackTeardownRequester(ctx context.Context) func() {
	pid := s.requesterPID
	if pid <= 0 {
		pid, _ = ctx.Value(httpPeerPIDContextKey{}).(int)
	}
	if pid <= 0 {
		return func() {}
	}
	process, err := proctree.Lookup(pid)
	if err != nil {
		return func() {}
	}
	return sessiontmux.TrackTeardownRequester(process)
}

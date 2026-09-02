package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type rpcRequesterContextKey struct{}

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

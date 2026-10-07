package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

type rpcRequesterContextKey struct{}

// httpPeerRequesterContextKey carries the kernel-verified identity of the
// process on the other end of a unix-socket HTTP connection — the
// SO_PEERCRED/LOCAL_PEERPID pid resolved to its (pid, start-stamp) instance by
// ConnContext when the connection is accepted (httpserver.go). It is the HTTP
// carrier of the same identity the control socket keeps in
// controlServer.requester — the kernel recorded it at connect time, so nothing
// in the request can mint it (#5182).
type httpPeerRequesterContextKey struct{}

// teardownReplyPendingContextKey carries the per-request pendingUntracks an
// HTTP handler's unregisters queue onto; rpcHandlerCtx drains it after the
// reply is flushed to the socket (#5182).
type teardownReplyPendingContextKey struct{}

// pendingUntracks holds requester unregistrations until the transport has
// provably finished with the reply, then runs them. The two drains: a control
// connection's ServeConn return — net/rpc serializes the reply inside the
// connection's lifetime, and a client only closes after reading or dying — and
// an HTTP request's response flush in rpcHandlerCtx. add() after drain runs
// inline, so an unregister posted by a still-finishing handler goroutine never
// dangles.
type pendingUntracks struct {
	mu      sync.Mutex
	drained bool
	fns     []func()
}

func (p *pendingUntracks) add(f func()) {
	p.mu.Lock()
	if p.drained {
		p.mu.Unlock()
		f()
		return
	}
	p.fns = append(p.fns, f)
	p.mu.Unlock()
}

func (p *pendingUntracks) drain() {
	p.mu.Lock()
	p.drained = true
	fns := p.fns
	p.fns = nil
	p.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

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
// The identity comes from the connection, never from request fields: on the
// control socket the accept loop resolved SO_PEERCRED/LOCAL_PEERPID's pid to a
// (pid, start-stamp) instance stored in controlServer.requester; over the
// unix-socket HTTP API ConnContext resolved the same answer onto the request
// context. A client-supplied pid cannot reach this code path at all, so a
// process cannot claim an exemption it is not owed (the issue's property (d)).
//
// Resolution happened at ACCEPT, while the peer provably still owned its pid
// slot: a caller that exited and had its pid recycled between connect and this
// handler left behind an identity no live process matches, so a recycled
// pid's new occupant fails the start-stamp match rather than inheriting the
// exemption. Anything that could not produce a verified identity — a read
// failure, an unsupported platform, a peer already gone at accept — carries
// no requester, which is exactly the pre-#5182 behavior.
func (s *controlServer) trackTeardownRequester(ctx context.Context) func() {
	requester := s.requester
	if requester == nil {
		requester, _ = ctx.Value(httpPeerRequesterContextKey{}).(*proctree.Process)
	}
	if requester == nil {
		return func() {}
	}
	untrack := sessiontmux.TrackTeardownRequester(*requester)
	// The handler returning is NOT the end of the caller's exposure: the
	// transport serializes the reply AFTER the service method returns, and a
	// concurrent teardown's signal tier could land in that gap and kill the
	// requester while its answer is still queued (Codex on #5186). The commit
	// therefore parks the unregister on whichever completion the transport
	// exposes rather than running it here — the connection's ServeConn return
	// for net/rpc, the response flush for HTTP — so the registration outlives
	// the reply itself, not a guess about how long the write takes. With no
	// such hook (a unit test driving a handler directly), the unregister runs
	// at once: the pre-#5182 boundary.
	var once sync.Once
	return func() {
		once.Do(func() {
			if p, ok := ctx.Value(teardownReplyPendingContextKey{}).(*pendingUntracks); ok && p != nil {
				p.add(untrack)
				return
			}
			if s.pendingReplies != nil {
				s.pendingReplies.add(untrack)
				return
			}
			untrack()
		})
	}
}

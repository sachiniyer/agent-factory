package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/session"
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
// provably finished with the reply, then runs them. Keyed entries are parked
// under the request argv pointer net/rpc decoded for their call — the codec
// maps each response's Seq back to that argv, so a response can only release
// the exemption the exact call that created it is waiting on, on success and
// on handler error alike (sendResponse swaps the reply body for
// invalidRequest; argv is stable). A Ping, a decode-rejected request, or a
// teardown call refused before it ever registered releases nothing. Unkeyed
// entries ride the HTTP per-request queue and are drained wholesale by
// rpcHandlerCtx after the reply flush. Close/drain after the connection dies
// releases whatever is left: a reply the client will never read must not
// exempt the requester forever. add/addFor after drain runs inline, so an
// unregister posted by a still-finishing handler goroutine never dangles.
type pendingUntracks struct {
	mu      sync.Mutex
	drained bool
	keyed   map[any]func()
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

func (p *pendingUntracks) addFor(key any, f func()) {
	p.mu.Lock()
	if p.drained {
		p.mu.Unlock()
		f()
		return
	}
	if p.keyed == nil {
		p.keyed = make(map[any]func())
	}
	p.keyed[key] = f
	p.mu.Unlock()
}

// releaseFor runs the unregister parked under key — the request argv pointer
// of the call whose response the transport just wrote (#5182).
func (p *pendingUntracks) releaseFor(key any) {
	p.mu.Lock()
	f, ok := p.keyed[key]
	if ok {
		delete(p.keyed, key)
	}
	p.mu.Unlock()
	if ok {
		f()
	}
}

func (p *pendingUntracks) drain() {
	p.mu.Lock()
	p.drained = true
	fns := p.fns
	p.fns = nil
	keyed := p.keyed
	p.keyed = nil
	p.mu.Unlock()
	for _, f := range fns {
		f()
	}
	for _, f := range keyed {
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
// process for the handler's duration and returns the unregister (#5182). Only
// KillSession and ArchiveSession call it — the two teardown verbs behind all
// measured #5182 cases; handoff, resume-from-limit, project delete,
// config-agent reap, and tab-close are the same exposure and land in #5193,
// which this seam is already shaped for (register + pointer arg + ctx route).
//
// requestKey is the handler's request pointer — the argv value net/rpc decoded
// for this call; on a control connection the parked unregister is keyed by it
// so ONLY the transport write of this call's own response releases the
// exemption — a multiplexed sibling response, or an error response whose body
// was swapped for invalidRequest, can never free or strand a registration it
// does not own (gobServerCodec maps seq→argv). HTTP callers pass nil: their
// per-request queue drains wholesale after the flush, which is already scoped
// to this call.
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
// targets is the teardown this call will own — the tmux session names and
// worktree paths the handler resolved for its request. The registry scopes
// the exemption to THOSE teardowns (#7754): a requester blocked on archiving
// session B while living in session A's pane is excused from B's reap only,
// and A's teardown counts it among A's survivors like any other claimant.
// Nil targets register nothing — an unresolvable or soon-to-be-refused
// request earns no exemption, the same as an unverifiable identity.
func (s *controlServer) trackTeardownRequester(ctx context.Context, requestKey any, targets []teardownreq.Target) func() {
	requester := s.requester
	if requester == nil {
		requester, _ = ctx.Value(httpPeerRequesterContextKey{}).(*proctree.Process)
	}
	if requester == nil {
		return func() {}
	}
	untrack := sessiontmux.TrackTeardownRequester(*requester, targets...)
	// Disconnect release, parked NOW rather than at handler return: a peer
	// that dies while this handler is still running cannot be waiting for any
	// reply, so its exemption ends at disconnect — the codec's EOF drain (and
	// the request-context cancellation for HTTP) runs this unkeyed entry. The
	// keyed/addFor entry below still governs the success path and whichever
	// fires first wins; teardownreq's unregister is once-guarded so the other
	// is a no-op (Codex on #5186).
	if p, ok := ctx.Value(teardownReplyPendingContextKey{}).(*pendingUntracks); ok && p != nil {
		p.add(untrack)
		// net/http cancels the request context when the client's connection
		// drops; drain then, not only after the handler returns.
		go func() {
			<-ctx.Done()
			p.drain()
		}()
	} else if s.pendingReplies != nil {
		s.pendingReplies.add(untrack)
	}
	// The handler returning is NOT the end of the caller's exposure: the
	// transport serializes the reply AFTER the service method returns, and a
	// concurrent teardown's signal tier could land in that gap and kill the
	// requester while its answer is still queued (Codex on #5186). The commit
	// therefore parks the unregister on whichever completion the transport
	// exposes rather than running it here — keyed by this call's request argv
	// for net/rpc (gobServerCodec.WriteResponse releases only that entry), the
	// response flush for HTTP — so the registration outlives the reply itself,
	// not a guess about how long the write takes. With no such hook (a unit
	// test driving a handler directly), the unregister runs at once: the
	// pre-#5182 boundary.
	var once sync.Once
	return func() {
		once.Do(func() {
			if p, ok := ctx.Value(teardownReplyPendingContextKey{}).(*pendingUntracks); ok && p != nil {
				p.add(untrack)
				return
			}
			if s.pendingReplies != nil {
				if requestKey != nil {
					s.pendingReplies.addFor(requestKey, untrack)
					return
				}
				s.pendingReplies.add(untrack)
				return
			}
			untrack()
		})
	}
}

// teardownTargetsForSession resolves the session a destructive request names
// to the targets its teardown will own (#7754): the tmux session names it
// will close and the worktree paths its writer reaper will sweep — for a live
// instance including the archive destination a rollback reaps (#7754 R1) and
// the recovery alternate a claim may select (R4), and for a disk-only ghost
// the names and paths the persisted record carries (R2). The request is
// resolved through the SAME resolver the operation itself uses
// (resolveActionSession, id-first), so the registered target is the session
// the teardown actually reaches — not the request's possibly stale title.
//
// Only a request that resolves NOTHING — a bad name, a session already gone
// — carries no targets: the registration is empty, which exempts nothing.
// That is the safe direction both ways: the teardown the request could not
// name cannot be mis-attributed to it, and a refused request leaves its
// caller's pane exactly as killable as before #5182.
//
// Resolution and the operation's own resolve can still race the map between
// here and there: a same-title replacement created in the gap tears the NEW
// instance down under names this set does not hold, and the requester then
// earns no exemption for it — it is reaped like any other captured process.
// That is deliberately the conservative edge: an over-wide target would
// excuse a caller from a teardown it never asked for.
func (m *Manager) teardownTargetsForSession(id, title, repoID string) []teardownreq.Target {
	instance, resolvedRepoID, resolvedTitle, _, data, err := m.resolveActionSession(id, title, repoID)
	if err != nil {
		return nil
	}
	switch {
	case instance != nil:
		targets := instance.TeardownTargets()
		// A worktree archive MOVES the tree to a destination under the
		// archive dir, and a rollback of that move sweeps writers at the
		// destination — inside the very teardown the requester is blocked on
		// (#7754 R1): a caller cwd'd in the worktree follows the directory
		// there and would read as a survivor at an unregistered path. The
		// destination derives from the RESOLVED identity, the same title the
		// operation canonicalizes to before computing it. It is inert for a
		// kill, whose reapers never sweep the archive dir.
		if dest, derr := archivedWorktreePath(resolvedRepoID, resolvedTitle); derr == nil && dest != "" {
			targets = append(targets, teardownreq.WorktreeTarget(dest))
		}
		return targets
	case data != nil:
		// A disk-only record is not a refusal: the ghost kill goes ahead and
		// runs the real reapers on the names and paths the record carries
		// (#7754 R2), so the exemption must be scoped to exactly them.
		return ghostTeardownTargets(data)
	default:
		return nil
	}
}

// ghostTeardownTargets is the disk-record twin of Instance.TeardownTargets
// (#7754 R2). A ghost teardown has no live instance to snapshot; the record
// IS the authority on what the cleanup will reap — ghostTmuxNames is the same
// enumeration ghostCleanup drives its tmux kills from, and the worktree
// candidates are the spellings the reconstructed GitWorktree can select.
func ghostTeardownTargets(data *session.InstanceData) []teardownreq.Target {
	names := ghostTmuxNames(data)
	targets := make([]teardownreq.Target, 0, len(names))
	for _, name := range names {
		targets = append(targets, teardownreq.TmuxTarget(name))
	}
	add := func(path string) {
		if path != "" {
			targets = append(targets, teardownreq.WorktreeTarget(path))
		}
	}
	add(data.Worktree.WorktreePath)
	if recovery := data.Worktree.RelocationRecovery; recovery != nil {
		add(recovery.AlternatePath)
	}
	// The cleanup itself restores the record's ORIGINAL worktree spelling
	// before it reaps (ghostCleanupWorktree decodes the rollback fences), so
	// the on-wire spelling can differ from the path the reaper runs on. The
	// decoded candidate is registered too — a duplicate target is harmless.
	if decoded, err := data.RestoreArchiveRollbackFence().RestoreRelocationRecoveryOriginals(); err == nil {
		add(decoded.Worktree.WorktreePath)
		if recovery := decoded.Worktree.RelocationRecovery; recovery != nil {
			add(recovery.AlternatePath)
		}
	}
	return targets
}

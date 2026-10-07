package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// webListeners owns the daemon's two restartable TCP listeners — the control-plane
// web listener (network.listen_addr) and the web-tab preview listener (network.preview_listen_addr)
// — so #2480 PR2 can apply a network.listen_addr / network.preview_listen_addr change in place
// without a daemon restart. It is the ONE bind path: startHTTPServer does the
// initial bind through it, and ApplyConfig's reconcile does the rebinds, so the
// two can never drift.
//
// It handles ONLY the two socket keys. The auth/CORS keys
// (network.require_token / network.require_loopback_token / network.cors_allowed_origins) read live config
// per request (livePosture) and never rebind. Two reasons, and the first is a
// CONSTRAINT — much harder for a future refactor to argue away than a preference:
//
//  1. Mechanically, they CANNOT rebind. The common change — network.require_token flipped
//     with network.listen_addr UNCHANGED — would have to bind a new listener on the SAME
//     address the old one still holds, which fails with "address already in use".
//     The only way to bind that same port is to close the old FIRST — the exact
//     close-then-bind ordering that bricks the daemon when the new bind then fails.
//     So there is no bind-new-before-close available for a same-address posture
//     change; the auth keys must apply WITHOUT touching the socket.
//  2. And it is the right coupling anyway. Routing a security tightening through the
//     socket path would couple it to a bind succeeding, and a failed bind keeps the
//     OLD, weaker posture serving — a tighten that silently fails permissive.
//     Live-read applies the tighten on the next request whether or not any rebind
//     here succeeds.
type webListeners struct {
	manager *Manager
	// webMux is the shared control-plane mux (also served on the unix socket).
	webMux http.Handler
	// previewMux is the web-tab preview listener's OWN mux: one catch-all route that
	// resolves a tab from the request's per-tab host label and proxies it (#1856). It
	// is deliberately NOT webMux — the preview origin serves previews only and never
	// the control API — and it is built once, like webMux, so a rebind cannot hand the
	// two listeners different handler graphs.
	previewMux http.Handler
	// listenTCP is the bind boundary for both restartable listeners. Keeping it
	// on the owner lets lifecycle tests distinguish an underlying listener death
	// from the owner's server-close path while exercising the real HTTP server.
	listenTCP func(network, address string) (net.Listener, error)

	mu sync.Mutex
	// webHandle owns all accepted connections for the latest generation, so an
	// unexpected Serve exit does not make it stale. webConfigAddr describes only
	// the live binding: it is the CONFIG value that produced the listener (not the
	// resolved bound address), so reconcile compares like-for-like. "" means not
	// accepting (or the network.listen_addr="" opt-out).
	webHandle     *tcpListenerHandle
	webConfigAddr string
	// webRefusal is the refusal reason currently in force for the control
	// listener (#5137): non-empty while the configured posture is one the daemon
	// refuses to bind (non-loopback + tokenless + no opt-in). It doubles as the
	// idempotence key — reconcile re-runs for every apply, but the refusal is
	// re-recorded (and the ERROR re-logged) only when the reason changes, so an
	// unrelated save under a refused posture does not spam the log. Cleared by
	// every path that leaves the refused posture: a successful bind, the opt-out
	// teardown, or a posture the predicate no longer refuses.
	webRefusal string
	// webBoundAddr is the RESOLVED address that binding is accepting on — what
	// ":0" or ":8443" actually became. It is what a save surface must report back
	// to an operator who just moved the listener (#3722): the config value alone
	// does not name a port the kernel chose, and it is the wrong thing to echo
	// after a rebind FAILED, where config has already moved on and the daemon is
	// still answering on the previous address. Kept beside webConfigAddr so both
	// are cleared by the same paths.
	webBoundAddr string
	// webGen distinguishes listener generations so a superseded listener's Serve
	// returning cannot clear the lifecycle bound-state the NEW listener just set. A
	// done-watcher clears health and binding state only while its own generation is
	// still current, allowing an unchanged configured address to be rebound. It
	// clears the handle only after a teardown WE asked for initiated the exit
	// (close or retire, both of which set closeRequested); after listener failure
	// the handle remains the owner of the accepted connections.
	webGen uint64

	previewHandle     *tcpListenerHandle
	previewConfigAddr string
	previewBoundAddr  string
	previewGen        uint64

	// webTracker tracks the control listener's hijacked (WebSocket) conns
	// across ALL generations. Per-generation it would be a lie: a rebind
	// retires the old handle while its hijacked streams stay open by design,
	// so a refusal arriving generations later must reach every conn the kind
	// is still serving — the retired generations' included. Severs are called
	// only from the policy-retire paths; the preview listener has none, so it
	// gets no tracker.
	webTracker *connTracker
}

// newWebListeners builds the manager (never binds — startHTTPServer's initial
// bind and ApplyConfig's rebinds both go through reconcileFromLocked/bind* below).
func newWebListeners(manager *Manager, webMux, previewMux http.Handler) *webListeners {
	return &webListeners{manager: manager, webMux: webMux, previewMux: previewMux, listenTCP: net.Listen, webTracker: newConnTracker()}
}

// reconcile brings the two socket listeners in line with newCfg, rebinding only
// the one whose CONFIG address changed (bind-new-before-close, with a
// release-then-bind fallback for the same-port overlap shape of #5140). It never
// touches the auth/CORS posture — that is live-read per request. It returns the
// socket keys whose rebind FAILED (the current listener is left serving for each)
// and the joined error naming each address and reason, so a save surface can
// report the change as deferred rather than silently dropping it.
func (wl *webListeners) reconcile(newCfg *config.Config) (failed []string, err error) {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	// A handle retained after unexpected listener death outlives the empty
	// binding sentinel. Disabling that listener must still enter bindWebLocked's
	// teardown path so accepted connections do not survive reconciliation.
	//
	// A failed initial bind leaves webHandle==nil and webConfigAddr=="" while the
	// lifecycle still records the boot-time address as TCPConfigured=true /
	// TCPListenAddr=<addr> (set by newDaemonLifecycle independently). When the
	// operator then disables the listener, the first two conditions are both false
	// — "" == "" and webHandle is nil — so without the third condition the
	// lifecycle pair is never cleared and status keeps reporting the old address as
	// "not bound" for the rest of the boot. Enter the teardown path whenever the
	// lifecycle configured half is stale so bindWebLocked can clear it.
	lcfg := func() DaemonListenerStatus {
		if wl.manager.lifecycle == nil {
			return DaemonListenerStatus{}
		}
		return wl.manager.lifecycle.snapshot().listeners
	}()
	var webErr, previewErr error
	// #5137: a non-loopback listen_addr with the token off and no explicit
	// allow_unauthenticated_network opt-in is a posture the daemon REFUSES to
	// bind. The refusal is checked before the address-change test, not as an
	// error inside the bind: it is policy, not a bind failure, so it does not
	// join `failed`/`errs` — nothing is deferred to a next start that would
	// refuse again, and the save surface gets the reason through the
	// apply-time warning instead. It runs on EVERY reconcile (not only when the
	// address changed): an auth-posture edit can withdraw the opt-in under a
	// serving listener, and that listener must retire here.
	refusal := config.ListenerBindRefusal(newCfg)
	if refusal != "" {
		if wl.webRefusal != refusal || wl.webHandle != nil {
			wl.refuseWebLocked(newCfg.ListenAddr, refusal)
		}
	} else if newCfg.ListenAddr != wl.webConfigAddr ||
		(newCfg.ListenAddr == "" && wl.webHandle != nil) ||
		(newCfg.ListenAddr == "" && lcfg.TCPConfigured) {
		webErr = wl.bindWebLocked(newCfg.ListenAddr)
	}
	if newCfg.PreviewListenAddr != wl.previewConfigAddr ||
		(newCfg.PreviewListenAddr == "" && wl.previewHandle != nil) ||
		(newCfg.PreviewListenAddr == "" && lcfg.PreviewConfigured) {
		previewErr = wl.bindPreviewLocked(newCfg.PreviewListenAddr)
	}
	// A bind the gate declined for SIBLING overlap was decided against the
	// sibling's OLD bound address — and this same apply may have just moved or
	// torn the sibling down. Retry each declined bind once now that both steps
	// have run: a sibling that left the port frees the blocker entirely.
	//
	// The exception is the TWO-WAY swap: when BOTH binds came back
	// sibling-declined, each request names the address the other listener is
	// holding, so no single release frees a port and plain retries repeat the
	// same conflict forever. If the two requests can coexist — the swap shape —
	// retire both listeners and bind each in turn; a bind that still fails
	// rolls that listener back to the address it just left, the same rollback
	// the single-listener release path runs. When the requests OVERLAP each
	// other (both sides asked for the same wildcard, say) the mutual decline is
	// not a swap at all — releasing both would strand one listener — so the
	// errors stand and both changes report deferred with both handles intact.
	var sb siblingBlockedError
	webSB := errors.As(webErr, &sb)
	previewSB := errors.As(previewErr, &sb)
	if webSB && previewSB && listenRequestsCoexist(listenErrAddr(webErr, newCfg.ListenAddr), listenErrAddr(previewErr, newCfg.PreviewListenAddr)) {
		webErr, previewErr = wl.swapWebPreviewLocked(newCfg)
	} else {
		if webSB {
			webErr = wl.bindWebLocked(newCfg.ListenAddr)
		}
		if previewSB {
			previewErr = wl.bindPreviewLocked(newCfg.PreviewListenAddr)
		}
	}
	var errs []error
	if webErr != nil {
		errs = append(errs, webErr)
		failed = append(failed, "network.listen_addr")
	}
	if previewErr != nil {
		errs = append(errs, previewErr)
		failed = append(failed, "network.preview_listen_addr")
	}
	return failed, errors.Join(errs...)
}

// retireWebBeforePostureSwap enforces the #5137 refusal BEFORE ApplyConfig's
// live-config swap publishes newCfg. The ordering is the point: the auth keys
// are live-posture — livePosture reads the swapped config per request — so the
// moment the swap lands, a still-bound socket answers under the NEW posture,
// while reconcile (which owns the refusal bookkeeping) runs only afterward.
// Between the two, a socket the incoming config renders refused would serve
// the full control API unauthenticated on a network interface: a hand edit
// that flips network.require_token off under a bound network listener, or a
// retained socket whose failed rebind left it answering on an address the file
// no longer names. Retiring here — before the publish — closes that window.
//
// Two judgments:
//
//   - The CONFIGURED posture refused (non-loopback network.listen_addr, token
//     off, no opt-in): refuseWebLocked retires the socket AND records the
//     refusal — the same work reconcile does post-swap, run early under the
//     same webRefusal idempotence gate, so the ERROR still logs once per
//     distinct refusal.
//   - The configured posture allowed but the still-BOUND socket refused: a
//     failed rebind deliberately retains the previous listener, so the file
//     can name a safe loopback while the socket answering is still the old
//     network bind — and the swap is about to strip its token requirement.
//     That socket is retired WITHOUT recording a refusal: the configured
//     address is allowed, and reconcile may still bind it, so status must not
//     report a refusal the file never asked for.
func (wl *webListeners) retireWebBeforePostureSwap(newCfg *config.Config) {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	if refusal := config.ListenerBindRefusal(newCfg); refusal != "" {
		if wl.webRefusal != refusal || wl.webHandle != nil {
			wl.refuseWebLocked(newCfg.ListenAddr, refusal)
		}
		return
	}
	if wl.webHandle == nil {
		return
	}
	// Judge the SERVING socket under the incoming auth posture: webConfigAddr
	// is the config value that produced it, which a failed rebind deliberately
	// leaves diverged from the file's network.listen_addr. A file that now
	// names loopback does not make a still-bound network socket safe to carry
	// into a tokenless posture.
	serving := *newCfg
	serving.ListenAddr = wl.webConfigAddr
	if config.ListenerBindRefusal(&serving) == "" {
		return
	}
	addr := wl.webBoundAddr
	if addr == "" {
		addr = wl.webConfigAddr
	}
	wl.webHandle.retire()
	// Same severing as refuseWebLocked: this retire exists because the socket's
	// incoming posture is one it must not serve, and a hijacked stream opened
	// under the tokened posture would otherwise keep working it unauthenticated
	// — retire's drain does not reach hijacked connections.
	wl.webTracker.sever()
	wl.webHandle = nil
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.clearTCPBound()
	}
	// Not accepting and not configured-for-bind, same as refuseWebLocked's
	// teardown half: webConfigAddr/webBoundAddr clear so the reconcile below
	// sees the configured address as a fresh bind to attempt, and status never
	// echoes a bound address that no longer serves.
	wl.webConfigAddr = ""
	wl.webBoundAddr = ""
	wl.webGen++
	if n := wl.manager.sandboxTokens.revokeAll(); n > 0 {
		log.WarningLog.Printf("control listener on %s retired before the tokenless posture applied: revoked %d sandbox callback credential(s) minted against it; those sessions lose callback until a listener is bound and they are re-provisioned", addr, n)
	}
	log.ErrorLog.Printf("the control listener still bound on %s is retired before its token requirement is lifted: that address is "+
		"network-reachable, and af refuses to carry it into an unauthenticated posture without network.allow_unauthenticated_network — "+
		"the configured network.listen_addr %q is unaffected and still applies in this apply", addr, newCfg.ListenAddr)
}

// bindWebLocked (re)binds the control-plane web listener to the config address
// addr, BIND-NEW-BEFORE-CLOSE. addr=="" tears it down (the network.listen_addr="" opt-out).
//
// The ordering is the whole safety property: a new listener is bound FIRST, and the
// old is closed ONLY after the new is serving. If the new bind fails — port taken,
// unbindable host, permission denied on a low port — the OLD listener is left
// serving and an actionable error naming the address and reason is returned. A
// failed rebind must never leave the daemon unreachable through the very API an
// operator would use to fix the address.
//
// One shape is exempt from bind-first because it CANNOT work (#5140): a move that
// keeps the PORT while the two binds overlap — a wildcard owns every address on
// its port, so narrowing 0.0.0.0:P to a specific :P or widening one back out has
// the new bind meet the old listener still holding that port and fail EADDRINUSE
// no matter how many times it retries. That pair takes the inverse order in
// rebindWebSamePortLocked: release, bind, roll back on failure.
// Caller holds wl.mu.
func (wl *webListeners) bindWebLocked(addr string) error {
	// Reaching bindWebLocked at all means the posture is not refused (#5137) —
	// reconcile checks ListenerBindRefusal first — so any recorded refusal is
	// stale: the opt-in landed, the token came on, or the address moved back to
	// loopback. Clear it here rather than at each call site.
	wl.webRefusal = ""
	if addr == "" {
		if wl.webHandle != nil {
			// RETIRED, not closed: the network.listen_addr="" opt-out is a config
			// write like any other, and it arrives on the very listener it turns
			// off, so a synchronous close here severs its own reply exactly as a
			// rebind did (#3722). Accept stops synchronously either way.
			wl.webHandle.retire()
			wl.webHandle = nil
			if wl.manager.lifecycle != nil {
				// Clear the bound half FIRST, then the configured half, so a
				// concurrent /v1/health snapshot reader can never observe the
				// bug's disable-direction signature (TCPConfigured=true while
				// TCPBound=false) between the two updates. The death closure
				// below clears the bound half only — this path additionally
				// clears the configured half because the operator asked for the
				// opt-out (network.listen_addr is now "").
				wl.manager.lifecycle.clearTCPBound()
			}
		}
		// Clear the lifecycle configured half unconditionally when addr=="", even
		// when webHandle is nil. A failed initial bind leaves webHandle==nil but
		// the lifecycle still holds the boot-time address as TCPConfigured=true /
		// TCPListenAddr=<addr> (set independently by newDaemonLifecycle). Clearing
		// here ensures the operator's disable intent is reflected in status whether
		// or not the initial bind ever succeeded.
		if wl.manager.lifecycle != nil {
			wl.manager.lifecycle.setTCPConfigured("")
		}
		wl.webConfigAddr = ""
		wl.webBoundAddr = ""
		// Tearing the listener DOWN is a listener change like any other, and this
		// early return used to skip both consequences of that (#3012 review): no
		// sweep ran, so credentials outlived the listener entirely, and the
		// generation did not advance. The opt-out is the MOST complete form of "the
		// endpoint is gone", so it cannot be the one path that reports nothing.
		//
		// The generation bump no longer feeds a mint check — that fence moved onto
		// the registry in #3065, because a listener counter could not see an
		// auth-only invalidation. It stays because it still retires THIS generation:
		// the done-watcher below clears listener state only while its own generation
		// is current, and a torn-down listener must not have its state cleared twice.
		wl.webGen++
		if n := wl.manager.sandboxTokens.revokeAll(); n > 0 {
			log.WarningLog.Printf("network.listen_addr is now empty: revoked %d sandbox callback credential(s) — the control listener is closed, so nothing can call back until it is re-enabled and those sessions are re-provisioned", n)
		}
		return nil
	}
	// Leaving the refused posture for an allowed bind: with no old socket
	// retained (webHandle==nil) nothing is serving, so the lifecycle's
	// configured half can honestly move to the address about to be attempted —
	// which also clears a recorded TCPRefusalReason, so a bind FAILURE below
	// reports "configured, not bound" instead of a stale refusal the posture
	// no longer carries. A retained socket skips this: a failed rebind keeps
	// the PREVIOUS configured address serving, and the configured half must
	// keep naming it.
	if wl.webHandle == nil && wl.manager.lifecycle != nil {
		wl.manager.lifecycle.setTCPConfigured(addr)
	}
	cfg := wl.manager.Config()
	// policy/notice are snapshotted only for the one-time enable banner below; under
	// livePosture{policyFromConfig:true} the gate derives network.require_token /
	// network.require_loopback_token live per request, so this value never enforces auth.
	policy := webListenerPolicy(cfg)
	notice := config.ListenerExposureNotice(cfg)
	handle, info, err := wl.webBind(addr)
	if err != nil {
		// The #5140 shape: the new bind failed EADDRINUSE on an address the
		// listener we already hold could be blocking — same port AND the two
		// binds overlap (a wildcard owns every address on its port, or the
		// spellings resolve to the same address). Only then is release-then-bind
		// worth the gap it buys: a different-port failure is never the old
		// listener's doing, and a same-port failure on a DISTINCT address —
		// 127.0.0.1:P → 127.0.0.2:P while a third party owns the latter — is a
		// pair that could have coexisted, so releasing would bounce a healthy
		// listener for a conflict it did not cause. The comparison runs on
		// webBoundAddr — the RESOLVED address — so a ":0" config measures
		// against the port the kernel actually chose.
		//
		// The SIBLING is checked independently: a request overlapping the
		// preview listener's bound address cannot be fixed by releasing ours —
		// whether or not ours overlaps too — but this same apply may move or
		// tear the sibling down, so it is marked for reconcile's one retry
		// rather than settled here.
		if errors.Is(err, syscall.EADDRINUSE) {
			req := listenErrAddr(err, addr)
			if wl.previewBoundAddr != "" && boundListenerBlocks(wl.previewBoundAddr, req) {
				return siblingBlockedError{fmt.Errorf("apply network.listen_addr %q: %w — daemon still serving on %s", addr, err, servingOn(wl.webConfigAddr))}
			}
			if wl.webHandle != nil && boundListenerBlocks(wl.webBoundAddr, req) {
				return wl.rebindWebSamePortLocked(addr, policy, notice)
			}
		}
		return fmt.Errorf("apply network.listen_addr %q: %w — daemon still serving on %s", addr, err, servingOn(wl.webConfigAddr))
	}
	wl.adoptWebListenerLocked(addr, handle, info)
	wl.announceWebListenerLocked(addr, info, policy, notice)
	return nil
}

// webBind is the control listener's raw bind: one startTCPListenerWithListen
// on the web mux under the current config, policy and live posture. Hoisted
// out of bindWebLocked so the swap path's rollback can re-bind a released
// listener without re-running the whole gate. Caller holds wl.mu.
//
// The cross-generation hijack tracker rides here rather than in the callers
// because EVERY path that creates a control listener must get it: the
// ordinary bind, the #5140 release-then-bind retry, the swap, and the
// rollback re-bind alike.
func (wl *webListeners) webBind(bindAddr string) (*tcpListenerHandle, tcpListenerInfo, error) {
	cfg := wl.manager.Config()
	return startTCPListenerWithListen(wl.webMux, bindAddr, cfg, webListenerPolicy(cfg), withWebShell, nil,
		&livePosture{
			snapshot:         wl.manager.Config,
			policyFromConfig: true,
			sandboxTokens:    &wl.manager.sandboxTokens,
		}, wl.webTracker, wl.listenTCP)
}

// adoptWebListenerLocked installs a freshly bound listener as THE control-plane
// listener: swaps it into the owner, advances the generation, updates the
// lifecycle bound state, starts the done-watcher, and retires the superseded
// listener when one is still held. It never revokes sandbox credentials or logs
// the banner — announceWebListenerLocked owns that half — so a rollback onto
// the previous address can adopt without declaring a move that never happened.
// Caller holds wl.mu.
func (wl *webListeners) adoptWebListenerLocked(addr string, handle *tcpListenerHandle, info tcpListenerInfo) {
	// New listener is live. Swap it in, update lifecycle to the new address, THEN
	// close the old — never before, or a same-host client races an unreachable gap.
	old := wl.webHandle
	wl.webHandle = handle
	wl.webConfigAddr = addr
	wl.webBoundAddr = info.Addr
	wl.webGen++
	gen := wl.webGen
	if wl.manager.lifecycle != nil {
		// Update the configured half BEFORE the bound half, so a concurrent
		// /v1/health snapshot reader can never observe the bug's enable-direction
		// signature (TCPConfigured=false while TCPBound=true) between the two
		// updates. setTCPConfigured takes the CONFIGured address (addr), while
		// setTCPBound takes the kernel-resolved one (info.Addr), so TCPListenAddr
		// stays the operator's value and TCPBoundAddr the concrete port — matching
		// the documented split (lifecycle.go:42-44). This runs only on the
		// bind-success branch; the failed-rebind branch above returns before it,
		// leaving the configured half at the previous serving value (the exact
		// `webConfigAddr` discipline this file keeps for the failed-rebind case).
		wl.manager.lifecycle.setTCPConfigured(addr)
		wl.manager.lifecycle.setTCPBound(info.Addr)
	}
	go func() {
		<-info.done
		wl.mu.Lock()
		revoked := 0
		if wl.webGen == gen {
			if wl.manager.lifecycle != nil {
				wl.manager.lifecycle.clearTCPBound()
			}
			if info.closeRequested() {
				wl.webHandle = nil
			}
			wl.webConfigAddr = ""
			wl.webBoundAddr = ""
			// The listener is gone whether or not anyone asked for it to be, so an
			// UNEXPECTED death has to advance the same state a rebind or a teardown
			// does (#3012 review). Without this, a listener that dies under
			// http.Server left credentials registered against an endpoint that no
			// longer accepts — contradicting the invariant this file states — and
			// left webGen unchanged, so a create racing the failure passed its
			// post-mint revalidation and shipped a URL nothing answers.
			//
			// This is the third path that had to learn the same rule. Rebind and
			// teardown were the two I wrote by hand; the listener simply dying was
			// the one I did not think of, which is the argument for the rule being
			// stated on the state itself rather than remembered at each site.
			wl.webGen++
			revoked = wl.manager.sandboxTokens.revokeAll()
		}
		wl.mu.Unlock()
		if revoked > 0 {
			log.WarningLog.Printf("the control listener on %s is no longer accepting: revoked %d sandbox callback credential(s) issued against it; those sessions lose callback until the listener is restored and they are re-provisioned", addr, revoked)
		}
	}()
	if old != nil {
		// The OLD listener is RETIRED, never closed (#3722). A remote config write
		// that moves network.listen_addr arrives ON the old listener, so closing it
		// from inside that handler destroys the connection its own 200 is about to
		// be written to — the operator is told a committed, applied write failed and
		// retries against an address the daemon has already left. retire() stops the
		// old address accepting right here (so nothing below or after it sees two
		// live control listeners) and drains the in-flight replies on its own
		// goroutine, under a deadline. Never Shutdown on THIS goroutine: it waits
		// for the very handler that is calling us.
		old.retire()
	}
}

// announceWebListenerLocked is the second half of a successful control-plane
// bind: every sandbox callback credential minted against the previous listener
// points at a closed address now, so they are revoked — and the enable banner
// with the bound address, bearer token, and posture notice is logged. A rollback
// onto the previous address does NOT call it: the listener ended where it
// started, so nothing minted against it moved, and the banner would claim a
// move that never happened. Caller holds wl.mu.
func (wl *webListeners) announceWebListenerLocked(addr string, info tcpListenerInfo, policy tokenGatePolicy, notice string) {
	// The listener moved, so every sandbox callback credential minted against the
	// old one now points at a closed address (#3012 review). Each sandbox has the
	// URL baked into the environment file written at provision time and nothing
	// rewrites it, so those tokens can no longer be used by the sandboxes holding
	// them. Revoke rather than leave live credentials aimed at nothing, and SAY so:
	// otherwise the capability vanishes silently inside sandboxes nobody is
	// watching. Same rule the registry already applies across a daemon restart —
	// a credential does not outlive the listener it was issued against. Affected
	// sessions regain callback when they are re-provisioned.
	if n := wl.manager.sandboxTokens.revokeAll(); n > 0 {
		log.WarningLog.Printf("network.listen_addr moved to %s: revoked %d sandbox callback credential(s) minted against the previous listener; those sessions lose callback until they are re-provisioned", addr, n)
	}
	// The enable banner + posture, logged once per bind (initial and rebind). The
	// bearer-token line is the operator's only channel to a network listener's
	// credential; the posture switch and the exposure notice explain who may connect
	// without a token. Snapshotted from cfg for the log — enforcement stays live.
	log.InfoLog.Printf("daemon HTTP TCP listener bound on %s (plain HTTP — terminate TLS at a proxy if needed)", info.Addr)
	log.InfoLog.Printf("  bearer token: %s", info.Token)
	switch {
	case notice != "":
		// The exposure warning REPLACES the tokenless banner line for a network bind
		// rather than joining it — saying the same thing twice is how a warning stops
		// being read. Reachable only under the opt-in since #5137: without it the
		// bind never reaches here (refuseWebLocked).
		log.WarningLog.Printf("%s", notice)
	case policy.tokenDisabled:
		log.InfoLog.Printf("  all peers connect with NO token (network.require_token defaults to false; set network.require_token = true to require auth)")
	case policy.loopbackExempt:
		log.InfoLog.Printf("  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above")
	case config.IsLoopbackListenAddr(addr):
		log.InfoLog.Printf("  network.require_loopback_token=true: every peer (loopback included) must present the token above")
	default:
		log.InfoLog.Printf("  listener is network-bound: every peer must present the token above, INCLUDING loopback-origin requests — a same-host reverse proxy is NOT exempt (front it and let the proxy pass the token, or set network.require_token=false only on a fully trusted network)")
	}
}

// refuseWebLocked records the #5137 refusal of the control-plane listener: the
// configured addr is a non-loopback bind with the token off and no explicit
// network.allow_unauthenticated_network opt-in. The listener is RETIRED if one
// is serving (the refusal applies live — an auth-posture edit can withdraw the
// opt-in under a bound socket), the lifecycle gets the refused signature, and
// the reason is logged at ERROR — the posture is configured, reachable-looking,
// and silently doing nothing without it.
//
// It returns no error because it never fails: refusing is not a bind attempt.
// The reason goes to the log once per distinct refusal — reconcile's
// webRefusal gate suppresses re-runs — rather than through errs, because a
// refused listener is not a deferred rebind: no next start changes the answer,
// and FailedListenerKeys would have a save surface claim exactly that.
// Caller holds wl.mu.
func (wl *webListeners) refuseWebLocked(addr, refusal string) {
	if wl.webHandle != nil {
		// Retire, not close — the posture flip can arrive ON this listener (a
		// remote config write over the TCP route), so a synchronous close severs
		// the reply carrying it, exactly like the opt-out above (#3722).
		wl.webHandle.retire()
		wl.webHandle = nil
		if wl.manager.lifecycle != nil {
			wl.manager.lifecycle.clearTCPBound()
		}
	}
	// Then sever what retire cannot, whether or not a handle exists RIGHT NOW:
	// hijacked WebSocket streams survive Shutdown AND Close (the server stopped
	// counting them at the upgrade) AND survive their generation — an opt-out
	// or rebind can retire the handle while its streams stay open in the
	// tracker. A refusal exists to STOP this listener serving, so an open PTY
	// or events stream on ANY generation must not keep working the refused
	// posture.
	wl.webTracker.sever()
	// Not accepting and not configured-for-bind: webConfigAddr stays "" so the
	// next allowed reconcile still sees an address change to bind, and
	// webBoundAddr stays "" so save surfaces never echo a bound address that is
	// refused. The configured half lives ONLY in the lifecycle (setTCPRefused),
	// which is where `af daemon status` distinguishes refused from never-tried.
	wl.webConfigAddr = ""
	wl.webBoundAddr = ""
	wl.webGen++
	wl.webRefusal = refusal
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.setTCPRefused(addr, refusal)
	}
	// Same rule as every other path that leaves the endpoint gone: credentials
	// minted against the listener do not outlive it.
	if n := wl.manager.sandboxTokens.revokeAll(); n > 0 {
		log.WarningLog.Printf("network.listen_addr %s is refused (unauthenticated network posture): revoked %d sandbox callback credential(s) minted against the listener it replaces; those sessions lose callback until a listener is bound and they are re-provisioned", addr, n)
	}
	log.ErrorLog.Printf("%s", refusal)
}

// bindPreviewLocked is bindWebLocked for the web-tab preview listener: same
// bind-new-before-close discipline (and the same #5140 release-then-bind escape
// for a same-port overlap), its own mux (previewMux), its own per-tab
// credential (previewOriginAuth), its own always-strict gate posture, and the
// previewOrigin posture — a forced-empty CORS allow-list (the cross-tab read
// isolation #1856 rests on), no control-plane path/method shortcuts, and framed
// denials. Caller holds wl.mu.
func (wl *webListeners) bindPreviewLocked(addr string) error {
	if addr == "" {
		if wl.previewHandle != nil {
			wl.previewHandle.retire()
			wl.previewHandle = nil
			if wl.manager.lifecycle != nil {
				// Clear bound first, then configured, for the same reason as the
				// control listener's opt-out: a concurrent snapshot reader must
				// never see PreviewConfigured=true while PreviewBound=false as a
				// stable post-apply state. The death closure below clears the
				// bound half only; this additionally clears the configured half
				// because the operator set network.preview_listen_addr to "".
				wl.manager.lifecycle.clearPreviewBound()
			}
		}
		// Clear the lifecycle configured half unconditionally when addr=="", even
		// when previewHandle is nil. A failed initial bind leaves previewHandle==nil
		// but the lifecycle still holds the boot-time address as
		// PreviewConfigured=true / PreviewListenAddr=<addr>. Clearing here ensures
		// the operator's disable intent is reflected in status regardless of
		// whether the initial bind ever succeeded.
		if wl.manager.lifecycle != nil {
			wl.manager.lifecycle.setPreviewConfigured("")
		}
		wl.previewConfigAddr = ""
		wl.previewBoundAddr = ""
		return nil
	}
	cfg := wl.manager.Config()
	notice := config.PreviewListenerExposureNotice(cfg)
	handle, info, err := wl.previewBind(addr)
	if err != nil {
		// Same #5140 gate as the control listener, symmetric in both directions:
		// a request overlapping the CONTROL listener's bound address is marked
		// for reconcile's post-sibling retry whether or not ours overlaps too,
		// and release-then-bind runs only when the conflict could be ours alone.
		if errors.Is(err, syscall.EADDRINUSE) {
			req := listenErrAddr(err, addr)
			if wl.webBoundAddr != "" && boundListenerBlocks(wl.webBoundAddr, req) {
				return siblingBlockedError{fmt.Errorf("apply network.preview_listen_addr %q: %w — daemon still serving preview on %s", addr, err, servingOn(wl.previewConfigAddr))}
			}
			if wl.previewHandle != nil && boundListenerBlocks(wl.previewBoundAddr, req) {
				return wl.rebindPreviewSamePortLocked(addr, notice)
			}
		}
		return fmt.Errorf("apply network.preview_listen_addr %q: %w — daemon still serving preview on %s", addr, err, servingOn(wl.previewConfigAddr))
	}
	wl.adoptPreviewListenerLocked(addr, handle, info)
	announcePreviewListenerLocked(info.Addr, notice)
	return nil
}

// previewBind is webBind for the preview listener: one
// startTCPListenerWithListen on the preview mux under the current config —
// previewOrigin posture and per-tab credential included. Caller holds wl.mu.
func (wl *webListeners) previewBind(bindAddr string) (*tcpListenerHandle, tcpListenerInfo, error) {
	cfg := wl.manager.Config()
	return startTCPListenerWithListen(wl.previewMux, bindAddr, cfg, previewListenerPolicy(cfg), previewShell, previewOriginAuth(wl.manager),
		&livePosture{snapshot: wl.manager.Config, policyFromConfig: false, previewOrigin: true,
			previewWarmingUp: func() bool { return !wl.manager.Ready() }}, nil, wl.listenTCP)
}

// adoptPreviewListenerLocked is adoptWebListenerLocked for the preview
// listener: swap the handle and addresses in, advance the generation, update
// the lifecycle bound state, start the done-watcher, and retire the superseded
// listener when one is still held. Caller holds wl.mu.
func (wl *webListeners) adoptPreviewListenerLocked(addr string, handle *tcpListenerHandle, info tcpListenerInfo) {
	old := wl.previewHandle
	wl.previewHandle = handle
	wl.previewConfigAddr = addr
	wl.previewBoundAddr = info.Addr
	wl.previewGen++
	gen := wl.previewGen
	if wl.manager.lifecycle != nil {
		// Configured before bound, same ordering rationale as the control
		// listener: a concurrent snapshot reader must not see PreviewConfigured=false
		// while PreviewBound=true. setPreviewConfigured takes the configured address;
		// setPreviewBound takes the kernel-resolved concrete address.
		wl.manager.lifecycle.setPreviewConfigured(addr)
		wl.manager.lifecycle.setPreviewBound(info.Addr)
	}
	go func() {
		<-info.done
		wl.mu.Lock()
		if wl.previewGen == gen {
			if wl.manager.lifecycle != nil {
				wl.manager.lifecycle.clearPreviewBound()
			}
			if info.closeRequested() {
				wl.previewHandle = nil
			}
			wl.previewConfigAddr = ""
			wl.previewBoundAddr = ""
		}
		wl.mu.Unlock()
	}()
	if old != nil {
		// Retired, not closed, for the same reason and by the same rule as the
		// control listener above (#3722). The preview listener is not the one a
		// config write arrives on, so nothing here severs its own reply — the two
		// paths are kept identical because a listener-lifetime rule that holds on
		// one of a pair and not the other is how this file got its scars (#3012):
		// whichever path is the exception is the one a later change reasons from.
		old.retire()
	}
}

// announcePreviewListenerLocked logs the preview listener's bind banner — the
// per-tab origin notice, plus the exposure warning when configured. Skipped on
// a rollback, like the control listener's announce: nothing moved.
func announcePreviewListenerLocked(boundAddr string, notice string) {
	log.InfoLog.Printf("%s", previewOriginBanner(boundAddr))
	if notice != "" {
		log.WarningLog.Printf("%s", notice)
	}
}

// previewConfigAddress returns the CONFIG address that produced the preview listener
// currently serving — not the one config merely asks for. The two diverge exactly
// when a live rebind FAILED: ApplyConfig has already swapped the requested config in
// while reconcile left the previous listener accepting, so a decision made from
// config would describe a listener that does not exist. "" when nothing is bound.
func (wl *webListeners) previewConfigAddress() string {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	return wl.previewConfigAddr
}

// webConfigAddress is previewConfigAddress for the control-plane listener: the
// CONFIG address that produced the listener currently accepting, not the one
// config merely asks for. Same divergence, same cause — a live rebind that failed
// leaves the old listener serving while ApplyConfig has already stored the new
// address. "" when nothing is bound.
func (wl *webListeners) webConfigAddress() string {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	return wl.webConfigAddr
}

// listenerAddress reports the address the daemon is ACCEPTING on right now for
// one listener config key — the resolved one, so a ":0" or ":8443" config value
// answers with the port a client can actually dial. "" for any other key, and for
// a listener that is not bound (the ""-opt-out, or a first bind that failed).
//
// It reads the bound address rather than re-deriving one from config because the
// two diverge exactly when it matters most (#3722): after a rebind FAILS, config
// already holds the address the operator asked for while the daemon is still
// answering on the previous one. A save surface that echoed config there would
// name an address nothing is listening on, in the one case where the operator is
// most likely to act on it.
//
// The key is canonicalized first: the flat alias ("listen_addr") rides the wire
// for version skew, so a caller can reach this with either spelling.
func (wl *webListeners) listenerAddress(key string) string {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	switch config.CanonicalConfigKey(key) {
	case "network.listen_addr":
		return wl.webBoundAddr
	case "network.preview_listen_addr":
		return wl.previewBoundAddr
	default:
		return ""
	}
}

// ListenerAddress is listenerAddress for callers outside the listener owner (the
// SetConfigValue/UnsetConfigValue handlers). Safe on a manager with no listeners
// — a unix-socket-only daemon answers "" for every key, which is the truth.
func (m *Manager) ListenerAddress(key string) string {
	if m == nil || m.webListeners == nil {
		return ""
	}
	return m.webListeners.listenerAddress(key)
}

// close tears down both listeners (daemon shutdown). Errors are joined so one
// listener's close failure does not hide the other's.
//
// This is the one caller that still closes IMMEDIATELY rather than retiring
// (#3722). Retirement exists so a listener the daemon is replacing can still
// flush the reply that replaced it; at shutdown the process itself is going away,
// there is no successor to flush into, and an asynchronous drain would only be a
// deadline the exit races.
func (wl *webListeners) close() error {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	var errs []error
	if wl.webHandle != nil {
		errs = append(errs, wl.webHandle.close())
		wl.webHandle = nil
	}
	if wl.previewHandle != nil {
		errs = append(errs, wl.previewHandle.close())
		wl.previewHandle = nil
	}
	return errors.Join(errs...)
}

// servingOn renders the address a failed rebind fell back to, for the error the
// operator reads — the previous config address, or a plain phrase when there was
// no listener to fall back to (a first bind that failed serves nothing).
func servingOn(configAddr string) string {
	if configAddr == "" {
		return "no web address (the previous bind was disabled or absent)"
	}
	return fmt.Sprintf("the previous address %q", configAddr)
}

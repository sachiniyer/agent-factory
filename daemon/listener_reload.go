package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

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
	// clears the closer only after our Close initiated the exit; after listener
	// failure the closer remains the handle to accepted connections.
	webGen uint64

	previewHandle     *tcpListenerHandle
	previewConfigAddr string
	previewBoundAddr  string
	previewGen        uint64
}

// newWebListeners builds the manager (never binds — startHTTPServer's initial
// bind and ApplyConfig's rebinds both go through reconcileFromLocked/bind* below).
func newWebListeners(manager *Manager, webMux, previewMux http.Handler) *webListeners {
	return &webListeners{manager: manager, webMux: webMux, previewMux: previewMux, listenTCP: net.Listen}
}

// reconcile brings the two socket listeners in line with newCfg, rebinding only
// the one whose CONFIG address changed (bind-new-before-close). It never touches
// the auth/CORS posture — that is live-read per request. It returns the socket keys
// whose rebind FAILED (the current listener is left serving for each) and the
// joined error naming each address and reason, so a save surface can report the
// change as deferred rather than silently dropping it.
func (wl *webListeners) reconcile(newCfg *config.Config) (failed []string, err error) {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	var errs []error
	// A closer retained after unexpected listener death outlives the empty
	// binding sentinel. Disabling that listener must still enter bindWebLocked's
	// teardown path so accepted connections do not survive reconciliation.
	if newCfg.ListenAddr != wl.webConfigAddr ||
		(newCfg.ListenAddr == "" && wl.webHandle != nil) {
		if e := wl.bindWebLocked(newCfg.ListenAddr); e != nil {
			errs = append(errs, e)
			failed = append(failed, "network.listen_addr")
		}
	}
	if newCfg.PreviewListenAddr != wl.previewConfigAddr ||
		(newCfg.PreviewListenAddr == "" && wl.previewHandle != nil) {
		if e := wl.bindPreviewLocked(newCfg.PreviewListenAddr); e != nil {
			errs = append(errs, e)
			failed = append(failed, "network.preview_listen_addr")
		}
	}
	return failed, errors.Join(errs...)
}

// bindWebLocked (re)binds the control-plane web listener to the config address
// addr, BIND-NEW-BEFORE-CLOSE. addr=="" tears it down (the network.listen_addr="" opt-out).
//
// The ordering is the whole safety property: a new listener is bound FIRST, and the
// old is closed ONLY after the new is serving. If the new bind fails — port taken,
// unbindable host, permission denied on a low port — the OLD listener is left
// serving and an actionable error naming the address and reason is returned. A
// failed rebind must never leave the daemon unreachable through the very API an
// operator would use to fix the address. Caller holds wl.mu.
func (wl *webListeners) bindWebLocked(addr string) error {
	if addr == "" {
		if wl.webHandle != nil {
			// RETIRED, not closed: the network.listen_addr="" opt-out is a config
			// write like any other, and it arrives on the very listener it turns
			// off, so a synchronous close here severs its own reply exactly as a
			// rebind did (#3722). Accept stops synchronously either way.
			wl.webHandle.retire()
			wl.webHandle = nil
			if wl.manager.lifecycle != nil {
				wl.manager.lifecycle.clearTCPBound()
			}
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
	cfg := wl.manager.Config()
	// policy/notice are snapshotted only for the one-time enable banner below; under
	// livePosture{policyFromConfig:true} the gate derives network.require_token /
	// network.require_loopback_token live per request, so this value never enforces auth.
	policy := webListenerPolicy(cfg)
	notice := config.ListenerExposureNotice(cfg)
	handle, info, err := startTCPListenerWithListen(wl.webMux, addr, cfg, policy, withWebShell, nil,
		&livePosture{
			snapshot:         wl.manager.Config,
			policyFromConfig: true,
			sandboxTokens:    &wl.manager.sandboxTokens,
		}, wl.listenTCP)
	if err != nil {
		return fmt.Errorf("apply network.listen_addr %q: %w — daemon still serving on %s", addr, err, servingOn(wl.webConfigAddr))
	}
	// New listener is live. Swap it in, update lifecycle to the new address, THEN
	// close the old — never before, or a same-host client races an unreachable gap.
	old := wl.webHandle
	wl.webHandle = handle
	wl.webConfigAddr = addr
	wl.webBoundAddr = info.Addr
	wl.webGen++
	gen := wl.webGen
	if wl.manager.lifecycle != nil {
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
		// being read (#2168: warn, never refuse).
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
	return nil
}

// bindPreviewLocked is bindWebLocked for the web-tab preview listener: same
// bind-new-before-close discipline, its own mux (previewMux), its own per-tab
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
				wl.manager.lifecycle.clearPreviewBound()
			}
		}
		wl.previewConfigAddr = ""
		wl.previewBoundAddr = ""
		return nil
	}
	cfg := wl.manager.Config()
	policy := previewListenerPolicy(cfg)
	notice := config.PreviewListenerExposureNotice(cfg)
	handle, info, err := startTCPListenerWithListen(wl.previewMux, addr, cfg, policy, previewShell, previewOriginAuth(wl.manager),
		&livePosture{snapshot: wl.manager.Config, policyFromConfig: false, previewOrigin: true,
			previewWarmingUp: func() bool { return !wl.manager.Ready() }}, wl.listenTCP)
	if err != nil {
		return fmt.Errorf("apply network.preview_listen_addr %q: %w — daemon still serving preview on %s", addr, err, servingOn(wl.previewConfigAddr))
	}
	old := wl.previewHandle
	wl.previewHandle = handle
	wl.previewConfigAddr = addr
	wl.previewBoundAddr = info.Addr
	wl.previewGen++
	gen := wl.previewGen
	if wl.manager.lifecycle != nil {
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
	log.InfoLog.Printf("%s", previewOriginBanner(info.Addr))
	if notice != "" {
		log.WarningLog.Printf("%s", notice)
	}
	return nil
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

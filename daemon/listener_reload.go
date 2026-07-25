package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// webListeners owns the daemon's two restartable TCP listeners — the control-plane
// web listener (listen_addr) and the web-tab preview listener (preview_listen_addr)
// — so #2480 PR2 can apply a listen_addr / preview_listen_addr change in place
// without a daemon restart. It is the ONE bind path: startHTTPServer does the
// initial bind through it, and ApplyConfig's reconcile does the rebinds, so the
// two can never drift.
//
// It handles ONLY the two socket keys. The auth/CORS keys
// (require_token / require_loopback_token / cors_allowed_origins) read live config
// per request (livePosture) and never rebind. Two reasons, and the first is a
// CONSTRAINT — much harder for a future refactor to argue away than a preference:
//
//  1. Mechanically, they CANNOT rebind. The common change — require_token flipped
//     with listen_addr UNCHANGED — would have to bind a new listener on the SAME
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
	// webMux is the shared control-plane mux (also served on the unix socket). The
	// preview listener builds its own mux fresh per bind (newPreviewMux is stateless).
	webMux http.Handler

	mu sync.Mutex
	// webClose/webConfigAddr describe the currently-bound web listener. webConfigAddr
	// is the CONFIG value that produced it (not the resolved bound address), so
	// reconcile compares like-for-like and does not rebind on an unchanged setting.
	// "" ⇒ not serving (the listen_addr="" opt-out).
	webClose      func() error
	webConfigAddr string
	// webGen distinguishes listener generations so a superseded listener's Serve
	// returning cannot clear the lifecycle bound-state the NEW listener just set. A
	// done-watcher clears only while its own generation is still current.
	webGen uint64

	previewClose      func() error
	previewConfigAddr string
	previewGen        uint64
}

// newWebListeners builds the manager (never binds — startHTTPServer's initial
// bind and ApplyConfig's rebinds both go through reconcileFromLocked/bind* below).
func newWebListeners(manager *Manager, webMux http.Handler) *webListeners {
	return &webListeners{manager: manager, webMux: webMux}
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
	if newCfg.ListenAddr != wl.webConfigAddr {
		if e := wl.bindWebLocked(newCfg.ListenAddr); e != nil {
			errs = append(errs, e)
			failed = append(failed, "listen_addr")
		}
	}
	if newCfg.PreviewListenAddr != wl.previewConfigAddr {
		if e := wl.bindPreviewLocked(newCfg.PreviewListenAddr); e != nil {
			errs = append(errs, e)
			failed = append(failed, "preview_listen_addr")
		}
	}
	return failed, errors.Join(errs...)
}

// bindWebLocked (re)binds the control-plane web listener to the config address
// addr, BIND-NEW-BEFORE-CLOSE. addr=="" tears it down (the listen_addr="" opt-out).
//
// The ordering is the whole safety property: a new listener is bound FIRST, and the
// old is closed ONLY after the new is serving. If the new bind fails — port taken,
// unbindable host, permission denied on a low port — the OLD listener is left
// serving and an actionable error naming the address and reason is returned. A
// failed rebind must never leave the daemon unreachable through the very API an
// operator would use to fix the address. Caller holds wl.mu.
func (wl *webListeners) bindWebLocked(addr string) error {
	if addr == "" {
		if wl.webClose != nil {
			_ = wl.webClose()
			wl.webClose = nil
			if wl.manager.lifecycle != nil {
				wl.manager.lifecycle.clearTCPBound()
			}
		}
		wl.webConfigAddr = ""
		return nil
	}
	cfg := wl.manager.Config()
	// policy/notice are snapshotted only for the one-time enable banner below; under
	// livePosture{policyFromConfig:true} the gate derives require_token /
	// require_loopback_token live per request, so this value never enforces auth.
	policy := webListenerPolicy(cfg)
	notice := config.ListenerExposureNotice(cfg)
	closer, info, err := startTCPListener(wl.webMux, addr, cfg, policy, withWebShell, nil,
		&livePosture{snapshot: wl.manager.Config, policyFromConfig: true})
	if err != nil {
		return fmt.Errorf("apply listen_addr %q: %w — daemon still serving on %s", addr, err, servingOn(wl.webConfigAddr))
	}
	// New listener is live. Swap it in, update lifecycle to the new address, THEN
	// close the old — never before, or a same-host client races an unreachable gap.
	old := wl.webClose
	wl.webClose = closer
	wl.webConfigAddr = addr
	wl.webGen++
	gen := wl.webGen
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.setTCPBound(info.Addr)
		go func() {
			<-info.done
			wl.mu.Lock()
			if wl.webGen == gen {
				wl.manager.lifecycle.clearTCPBound()
			}
			wl.mu.Unlock()
		}()
	}
	if old != nil {
		_ = old()
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
		log.InfoLog.Printf("  all peers connect with NO token (require_token defaults to false; set require_token = true to require auth)")
	case policy.loopbackExempt:
		log.InfoLog.Printf("  loopback peers (127.0.0.1/::1) connect with no token; network peers must present the token above")
	case config.IsLoopbackListenAddr(addr):
		log.InfoLog.Printf("  require_loopback_token=true: every peer (loopback included) must present the token above")
	default:
		log.InfoLog.Printf("  listener is network-bound: every peer must present the token above, INCLUDING loopback-origin requests — a same-host reverse proxy is NOT exempt (front it and let the proxy pass the token, or set require_token=false only on a fully trusted network)")
	}
	return nil
}

// bindPreviewLocked is bindWebLocked for the web-tab preview listener: same
// bind-new-before-close discipline, its own mux (newPreviewMux), its own per-tab
// credential (previewListenerAuth), and its own always-strict gate posture (only
// its CORS list is live). Caller holds wl.mu.
func (wl *webListeners) bindPreviewLocked(addr string) error {
	if addr == "" {
		if wl.previewClose != nil {
			_ = wl.previewClose()
			wl.previewClose = nil
			if wl.manager.lifecycle != nil {
				wl.manager.lifecycle.clearPreviewBound()
			}
		}
		wl.previewConfigAddr = ""
		return nil
	}
	cfg := wl.manager.Config()
	policy := previewListenerPolicy(cfg)
	notice := config.PreviewListenerExposureNotice(cfg)
	closer, info, err := startTCPListener(newPreviewMux(), addr, cfg, policy, previewShell, previewListenerAuth(wl.manager),
		&livePosture{snapshot: wl.manager.Config, policyFromConfig: false})
	if err != nil {
		return fmt.Errorf("apply preview_listen_addr %q: %w — daemon still serving preview on %s", addr, err, servingOn(wl.previewConfigAddr))
	}
	old := wl.previewClose
	wl.previewClose = closer
	wl.previewConfigAddr = addr
	wl.previewGen++
	gen := wl.previewGen
	if wl.manager.lifecycle != nil {
		wl.manager.lifecycle.setPreviewBound(info.Addr)
		go func() {
			<-info.done
			wl.mu.Lock()
			if wl.previewGen == gen {
				wl.manager.lifecycle.clearPreviewBound()
			}
			wl.mu.Unlock()
		}()
	}
	if old != nil {
		_ = old()
	}
	log.InfoLog.Printf("web-tab preview listener bound on %s (serves no content yet — preview routing lands in a later step)", info.Addr)
	if notice != "" {
		log.WarningLog.Printf("%s", notice)
	}
	return nil
}

// close tears down both listeners (daemon shutdown). Errors are joined so one
// listener's close failure does not hide the other's.
func (wl *webListeners) close() error {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	var errs []error
	if wl.webClose != nil {
		errs = append(errs, wl.webClose())
		wl.webClose = nil
	}
	if wl.previewClose != nil {
		errs = append(errs, wl.previewClose())
		wl.previewClose = nil
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

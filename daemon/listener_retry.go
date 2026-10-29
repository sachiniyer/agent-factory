package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// recordBindError mirrors a bind outcome into the lifecycle snapshot that
// `af daemon status` reads: the failure text while the attempt is failing, ""
// once it succeeds (or the listener is configured off).
func (wl *webListeners) recordBindError(web bool, err error) {
	lc := wl.manager.lifecycle
	if lc == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if web {
		lc.setTCPBindError(msg)
	} else {
		lc.setPreviewBindError(msg)
	}
}

// Backoff for the startup bind retry. Vars so tests can shrink them.
var (
	listenRetryInitial = time.Second
	listenRetryMax     = 30 * time.Second
)

// retryFailedBinds keeps re-attempting the configured listeners after a startup
// bind failure (#5571 W9.9, topo:T10): a daemon that races tailscaled for a
// tailnet IP, or starts before an interface exists, would otherwise serve no
// network address until someone restarted it. Each attempt is a plain reconcile
// against the CURRENT config, so it goes through the one bind path, with the
// same auth posture and exposure notice as any other bind — a retry can never
// produce a listener the initial bind would not have. It ends when every
// configured listener is bound, when the config no longer asks for one, or at
// shutdown. Idempotent: a second call while one runs is a no-op.
func (wl *webListeners) retryFailedBinds() {
	wl.mu.Lock()
	if wl.closed || wl.retryStop != nil {
		wl.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	wl.retryStop = stop
	wl.mu.Unlock()
	if lc := wl.manager.lifecycle; lc != nil {
		lc.setBindRetrying(true)
	}
	go func() {
		defer func() {
			wl.mu.Lock()
			if wl.retryStop == stop {
				wl.retryStop = nil
			}
			wl.mu.Unlock()
			if lc := wl.manager.lifecycle; lc != nil {
				lc.setBindRetrying(false)
			}
		}()
		delay := listenRetryInitial
		lastMsg := ""
		for attempt := 1; ; attempt++ {
			timer := time.NewTimer(delay)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			// The config snapshot and the reconcile are ONE step under
			// configApplyMu, the lock ApplyConfig holds across its own swap +
			// reconcile. Without it a retry could capture config A, let an apply
			// store and reconcile B, then bind A over it and exit, leaving the
			// obsolete address serving. Lock order matches ApplyConfig:
			// configApplyMu, then wl.mu inside reconcile.
			wl.manager.configApplyMu.Lock()
			_, err := wl.reconcile(wl.manager.Config())
			wl.manager.configApplyMu.Unlock()
			if err == nil {
				log.InfoLog.Printf("daemon listener bind recovered after %d retr%s", attempt, map[bool]string{true: "y", false: "ies"}[attempt == 1])
				return
			}
			// State transitions and every 10th repeat, not every backoff tick of a
			// host whose interface never comes up.
			if msg := err.Error(); msg != lastMsg || attempt%10 == 0 {
				log.WarningLog.Printf("daemon listener bind still failing (attempt %d, retrying): %v", attempt, err)
				lastMsg = msg
			}
			if delay *= 2; delay > listenRetryMax {
				delay = listenRetryMax
			}
		}
	}()
}

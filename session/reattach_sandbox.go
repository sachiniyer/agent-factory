package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/log"
)

// ErrReattachIneligible reports a row reattach does not apply to: not a live
// ssh/sandbox row, archived or tombstoned, already bound, or written before the
// runtime identity (#5571 W1.1) existed. Such rows keep today's inert path.
var ErrReattachIneligible = errors.New("session has no reattachable runtime identity")

// reattachProvisioner returns the provisioner of an ssh/sandbox backend that
// persisted a complete runtime identity, or nil.
func reattachProvisioner(b Backend) *sandboxProvisioner {
	var p *sandboxProvisioner
	switch be := b.(type) {
	case *sandboxBackend:
		p = be.provisioner
	case *sshBackend:
		p = be.provisioner
	}
	if p == nil || p.tokenRef == "" || p.remoteAddr == "" || p.sessionDir == "" || p.sshCmd == "" {
		return nil
	}
	return p
}

// ReattachSandbox re-binds a persisted ssh/sandbox row to its still-running
// agent-server after a daemon restart (#5571 W1.5): it re-dials the -L forward
// to the persisted remote address, presents the token re-derived from the
// persisted ref and the daemon's runtime-identity key, and — only if the
// agent-server answers an alive probe — installs the live client and starts
// transport supervision. No provision or launch RPC is sent, so the agent is
// neither restarted nor re-cloned.
//
// On any failure the transport is dropped and the row is left exactly as the
// loader produced it (inert, Lost), so a later attempt or the existing
// restore path still works. ErrReattachIneligible means nothing was tried.
func (i *Instance) ReattachSandbox() error {
	i.mu.Lock()
	b := i.backend
	eligible := i.remoteClient == nil && i.runtimeTeardown != nil && !i.userKilled &&
		!i.startupStateUnknown && !i.runtimeCleanupStateUnknown && i.liveness != LiveArchived
	i.mu.Unlock()
	p := reattachProvisioner(b)
	if !eligible || p == nil {
		return ErrReattachIneligible
	}

	key, err := runtimetoken.LoadKey()
	if err != nil {
		return fmt.Errorf("reattach %q: runtime-identity key unavailable: %w", i.Title, err)
	}
	token, err := runtimetoken.DeriveAgentServerToken(key, p.tokenRef)
	if err != nil {
		return fmt.Errorf("reattach %q: %w", i.Title, err)
	}

	// The callback (#5571 W3.6): re-register the credential the sandbox already
	// holds, then reopen the transport WITH its persisted -R. Neither needs the
	// sandbox's env file rewritten — the credential is re-derived, not reissued.
	callbackLost := i.restoreCallbackForReattach(p)

	var localAddr string
	if p.startTunnelFn != nil {
		localAddr, err = p.startTunnelFn(p.remoteAddr)
	} else {
		var withCallback bool
		localAddr, withCallback, err = p.openTunnelForReattach()
		if err == nil && !withCallback && p.callbackForwardPersisted() && callbackLost == "" {
			callbackLost = "the callback forward could not be re-established on reattach (the sandbox host may still hold the old forward's port); it is retried each time the connection is re-dialed"
		}
	}
	if err != nil {
		p.abandonUnadoptedTunnel()
		return fmt.Errorf("reattach %q: %w", i.Title, err)
	}
	endpoint := &AgentServerEndpoint{URL: "http://" + localAddr, Token: token}
	rc, err := newRemoteAgentClient(*endpoint, i.Title)
	if err != nil {
		p.abandonUnadoptedTunnel()
		return fmt.Errorf("reattach %q: %w", i.Title, err)
	}
	// The derived token is what proves identity: only the agent-server launched
	// with this session's file-supplied token accepts it.
	if _, err := (&remoteAgentServer{rc: rc}).Alive(); err != nil {
		p.abandonUnadoptedTunnel()
		return fmt.Errorf("reattach %q: the agent-server did not answer: %w", i.Title, err)
	}

	// Re-verify under i.mu that the row is still the one this attempt probed: a
	// restore or Lost recovery that ran while the probe was in flight has already
	// installed its own runtime, and binding the old one over it would strand
	// the replacement and point the row at a reaped runtime.
	i.mu.Lock()
	stillCurrent := i.remoteClient == nil && i.backend == b && !i.userKilled && i.liveness != LiveArchived
	i.mu.Unlock()
	if !stillCurrent {
		p.abandonUnadoptedTunnel()
		return ErrReattachIneligible
	}
	if err := i.bindProvisionResult(ProvisionResult{Backend: b, Endpoint: endpoint, Teardown: p.reap}); err != nil {
		p.abandonUnadoptedTunnel()
		return fmt.Errorf("reattach %q: %w", i.Title, err)
	}
	if p.startTunnelFn == nil {
		go p.superviseTunnel()
	}
	i.mu.Lock()
	if !i.started {
		i.started = true
	}
	if len(i.Tabs) == 0 {
		i.Tabs = []*Tab{newRemoteAgentTab()}
	}
	i.appendPendingMetadataTabsLocked()
	// The loader rewrote a live row to Lost because nothing was bound; the
	// runtime answered, so it is live again. The next status poll refines Ready
	// into the agent's real state.
	i.liveness = LiveReady
	// A callback that could not be brought back is said out loud on the row, not
	// left as an agent whose af verbs fail silently; an earlier recorded reason
	// (a degraded create) is the more precise one and is kept.
	if callbackLost != "" && i.callbackUnavailable == "" {
		i.callbackUnavailable = callbackLost
	}
	i.touchLocked()
	i.mu.Unlock()
	log.InfoLog.Printf("sandbox runtime: session %q reattached to its agent-server at %s", i.Title, endpoint.URL)
	return nil
}

// abandonUnadoptedTunnel drops a tunnel generation reattach opened but did not
// adopt. Unlike stopTunnel it does not latch the provisioner stopped, so the
// row can be reattached again later and its eventual reap still works.
func (p *sandboxProvisioner) abandonUnadoptedTunnel() {
	unregisterSandboxTunnel(p)
	p.reapMu.Lock()
	t := p.tunnel
	p.tunnel = nil
	p.reapMu.Unlock()
	killTunnel(t)
}

// callbackReverse is the persisted form of the provisioner's -R spec: the one
// forward the callback credential rides (W3.4 sets at most one), or "".
func (p *sandboxProvisioner) callbackReverse() string {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if len(p.reverseFwds) == 1 && p.callbackRef != "" {
		return p.reverseFwds[0]
	}
	return ""
}

// restoreCallbackIdentity re-binds the persisted callback identity into a
// provisioner rebuilt from a cleanup handle, so the first tunnel it opens
// (reattach) and every supervised re-dial after it carry the same -R. Both
// halves are required: a ref without its forward, or a forward without its ref,
// is not a callback worth restoring.
func (p *sandboxProvisioner) restoreCallbackIdentity(ref, reverse string) {
	if ref == "" || reverse == "" {
		return
	}
	p.reapMu.Lock()
	p.callbackRef, p.reverseFwds = ref, []string{reverse}
	p.reapMu.Unlock()
}

// callbackForwardPersisted reports whether this provisioner carries a callback
// forward (restored from the row or set at provision).
func (p *sandboxProvisioner) callbackForwardPersisted() bool {
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	return len(p.reverseFwds) > 0 && p.callbackRef != ""
}

// restoreCallbackForReattach re-registers the callback credential this row's
// sandbox already holds (W3.6). It returns the reason the callback is down, or
// "" when it is restored or the row never had one. The credential is
// re-derived from the persisted ref and the daemon's key, so nothing is written
// to the sandbox.
func (i *Instance) restoreCallbackForReattach(p *sandboxProvisioner) string {
	if !p.callbackForwardPersisted() {
		return ""
	}
	i.mu.RLock()
	creds := i.sandboxCreds
	i.mu.RUnlock()
	restorer, ok := creds.(CallbackRestorer)
	if !ok {
		return "the daemon attached no callback credential minter to this session, so its callback was not restored after the restart"
	}
	p.reapMu.Lock()
	ref := p.callbackRef
	p.reapMu.Unlock()
	if err := restorer.RestoreCallback(ref); err != nil {
		log.WarningLog.Printf("sandbox runtime: session %q: callback not restored on reattach: %v", i.Title, err)
		return "the callback credential could not be restored after the restart: " + err.Error()
	}
	return ""
}

// openTunnelForReattach opens the first transport generation with the persisted
// callback -R. The data plane is the point of a reattach, and ssh's
// ExitOnForwardFailure makes a -R whose remote port the sandbox host still
// holds (the old connection's sshd side not yet reaped) fatal to the whole
// generation. So on failure with a callback forward present it retries once
// WITHOUT it, reports that it did, and leaves the forward in place for the
// supervisor to re-establish on its next re-dial. The original error is the one
// returned when the data plane cannot open either way.
func (p *sandboxProvisioner) openTunnelForReattach() (localAddr string, withCallback bool, err error) {
	localAddr, err = p.openTunnel(p.remoteAddr)
	if err == nil {
		return localAddr, p.callbackForwardPersisted(), nil
	}
	if !p.callbackForwardPersisted() {
		return "", false, err
	}
	p.abandonUnadoptedTunnel()
	p.reapMu.Lock()
	saved := p.reverseFwds
	p.reverseFwds = nil
	p.reapMu.Unlock()
	localAddr, retryErr := p.openTunnel(p.remoteAddr)
	p.reapMu.Lock()
	p.reverseFwds = saved
	p.reapMu.Unlock()
	if retryErr != nil {
		return "", false, err
	}
	return localAddr, false, nil
}

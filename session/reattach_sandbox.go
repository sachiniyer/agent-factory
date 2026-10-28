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

	var localAddr string
	if p.startTunnelFn != nil {
		localAddr, err = p.startTunnelFn(p.remoteAddr)
	} else {
		localAddr, err = p.openTunnel(p.remoteAddr)
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

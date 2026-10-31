package session

import (
	"errors"
	"fmt"
	"time"

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

	// dial opens the transport to the recorded address and asks the agent-server
	// whether it is alive. aliveErr separates "the server did not answer over a
	// working transport" (restartable) from a transport that never opened.
	callbackConfirmed := false
	dial := func() (endpoint *AgentServerEndpoint, rc *remoteAgentClient, aliveErr, err error) {
		var localAddr string
		if p.startTunnelFn != nil {
			localAddr, err = p.startTunnelFn(p.remoteAddr)
			// The test seam has no -R to confirm: it stands in for a confirmed one.
			callbackConfirmed = true
		} else {
			// The old connection died with the daemon; a socket its sshd left behind
			// would fail the -R bind (ExitOnForwardFailure) for good.
			if p.callbackForwardPersisted() {
				p.clearStaleCallbackSocket()
			}
			var note string
			var withCallback bool
			localAddr, withCallback, note, err = p.openTunnelForReattach()
			if err == nil && note != "" && callbackLost == "" {
				callbackLost = note
			}
			callbackConfirmed = withCallback
		}
		if err != nil {
			p.abandonUnadoptedTunnel()
			return nil, nil, nil, err
		}
		endpoint = &AgentServerEndpoint{URL: "http://" + localAddr, Token: token}
		rc, err = newRemoteAgentClient(*endpoint, i.Title)
		if err != nil {
			p.abandonUnadoptedTunnel()
			return nil, nil, nil, err
		}
		// The derived token is what proves identity: only the agent-server launched
		// with this session's file-supplied token accepts it.
		if _, aerr := (&remoteAgentServer{rc: rc}).Alive(); aerr != nil {
			p.abandonUnadoptedTunnel()
			return nil, nil, aerr, nil
		}
		return endpoint, rc, nil, nil
	}
	endpoint, rc, aliveErr, err := dial()
	if err != nil {
		return fmt.Errorf("reattach %q: %w", i.Title, err)
	}
	// persistIdentity writes the provisioner's live identity (PID, address,
	// adoption debt) onto the row's cleanup handle. Every exit past a spawn goes
	// through it: a process af started must be durably recorded or a daemon
	// restart would launch another beside it.
	persistIdentity := func() error { return i.persistRuntimeIdentity(b, p) }
	adopt := p.adoptPending // a restarted server a previous attempt left unadopted
	if aliveErr != nil {
		// The host answered over a working transport but the agent-server did not:
		// relaunch it if — and only if — the host proves it dead (W1.4).
		rerr := p.restartAgentServer(token, persistIdentity)
		if p.adoptPending {
			_ = persistIdentity() // a process was spawned, whether or not the restart completed
		}
		if rerr != nil {
			return fmt.Errorf("reattach %q: the agent-server did not answer: %w (not restarted: %v)", i.Title, aliveErr, rerr)
		}
		log.InfoLog.Printf("sandbox runtime: session %q: restarted its crashed agent-server (pid %s, %s)", i.Title, p.remotePID, p.remoteAddr)
		// Nothing has asked this server to touch the workspace yet, so one this
		// attempt cannot even reach is stopped rather than left to answer a
		// later probe.
		if endpoint, rc, aliveErr, err = dial(); err != nil || aliveErr != nil {
			p.stopUnadoptedServer()
			_ = persistIdentity()
			return fmt.Errorf("reattach %q: the restarted agent-server did not answer: %w", i.Title, errors.Join(err, aliveErr))
		}
		adopt = true
	}
	// Staged shell/process rows (a daemon restart leaves only these) join the
	// roster the restarted server is handed; idempotent, so a retry is a no-op.
	i.materializeRestorableTabs()
	if adopt {
		// The surviving workspace is adopted through the ordinary restore path
		// (firstTimeSetup=false): Provision re-derives and proves the worktree
		// handle before Launch reattaches a live tmux session or re-spawns one
		// that is gone, so a pane that outlived the crash keeps its agent.
		//
		// From here the server may hold the workspace, so a failure NEVER stops
		// it: an ordinary agent-server destroys its worktree on SIGTERM. It stays
		// running and recorded (adoptPending), and the next reattach finishes the
		// adoption.
		// inst is set so Launch sends the EXACT recorded conversation id: a bare
		// server would omit it and the host could resume onto another one (#4797).
		srv := &remoteAgentServer{rc: rc, inst: i, restartAdoption: true}
		if aerr := errors.Join(srv.Provision(false), srv.Launch(false)); aerr != nil {
			p.abandonUnadoptedTunnel()
			return fmt.Errorf("reattach %q: adopting the restarted agent-server failed (it is left running with its workspace; the next reattach retries): %w", i.Title, aerr)
		}
		// The agent may have been respawned rather than reattached: settle that
		// before the endpoint is bound, so no observation precedes it.
		i.noteAdoptedRestart()
		p.adoptPending = false
		if perr := persistIdentity(); perr != nil {
			log.WarningLog.Printf("sandbox runtime: session %q adopted its restarted agent-server but could not record that: %v", i.Title, perr)
		}
	}

	// Re-verify under i.mu that the row is still the one this attempt probed: a
	// restore or Lost recovery that ran while the probe was in flight has already
	// installed its own runtime, and binding the old one over it would strand
	// the replacement and point the row at a reaped runtime.
	i.mu.Lock()
	stillCurrent := i.remoteClient == nil && i.backend == b && !i.userKilled && i.liveness != LiveArchived
	// Adopting the same runtime provisions nothing, so the recorded callback
	// state travels with it (#5974): bindProvisionResult installs the result's
	// reason verbatim, and an empty one would read as a healthy reprovision.
	// The skipped agent-config list is the same class of provision evidence
	// (#6337): nothing re-staged the account scope, so the recorded skips are
	// still true of it.
	priorCallbackReason := i.callbackUnavailable
	priorAgentConfigSkipped := append([]string(nil), i.agentConfigSkipped...)
	i.mu.Unlock()
	if !stillCurrent {
		p.abandonUnadoptedTunnel()
		return ErrReattachIneligible
	}
	if err := i.bindProvisionResult(ProvisionResult{Backend: b, Endpoint: endpoint, Teardown: p.reap, CallbackUnavailable: priorCallbackReason, AgentConfigSkipped: priorAgentConfigSkipped}); err != nil {
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
	// The loader rewrote a live row to Lost because nothing was bound. The
	// runtime answered, which proves reachability and nothing about what the
	// agent is doing: a daemon that restarted under a working agent must not
	// publish it as idle (ClassifyActivity and `af sessions watch` read this).
	// Running is the conservative claim; the agent-server's status sweep settles
	// it to the observed state — its first post-reattach capture is a Baseline
	// that preserves this value, and idle needs observed silence (#5973).
	i.liveness = LiveRunning
	// The callback state is preserved until positive evidence says otherwise
	// (the W3.6 health rule, #5974): a reason recorded at create or on an
	// earlier reattach stays unless THIS reattach restored the credential and
	// confirmed the forward; a callback that could not be brought back is said
	// out loud rather than left as an agent whose af verbs fail silently.
	switch {
	case callbackConfirmed && callbackLost == "" && p.callbackForwardPersisted():
		i.callbackUnavailable = ""
	case callbackLost != "" && i.callbackUnavailable == "":
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
	// The carrier is a socket inside the session's own directory; remembering it
	// lets the first dial and every re-dial clear one a killed sshd left behind.
	if p.sessionDir != "" {
		p.callbackSocket = p.sessionDir + "/" + sandboxCallbackSocketName
	}
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

// Bounds for confirming the callback -R after a reattach dial. Vars so tests
// can shrink them.
var (
	callbackForwardConfirmTimeout = 6 * time.Second
	callbackForwardConfirmPoll    = 250 * time.Millisecond
)

// confirmCallbackForward is positive evidence the -R was accepted (#5896). The
// local -L listener answers before ssh has processed the remote-forward
// replies, so a dial proves nothing about the -R: sshd's acceptance shows only
// as the callback socket existing on the sandbox host (the stale one was
// removed before the dial, so a socket now is the fresh one), and a rejection
// as the ExitOnForwardFailure exit of the ssh child. Polled because the reply
// is asynchronous. A var so tests can stand in for the sandbox host.
var confirmCallbackForward = func(p *sandboxProvisioner) error {
	p.reapMu.Lock()
	sock, child := p.callbackSocket, p.tunnel
	p.reapMu.Unlock()
	if sock == "" {
		return errors.New("the callback socket path on the sandbox host is unknown")
	}
	deadline := time.Now().Add(callbackForwardConfirmTimeout)
	var last error
	for {
		if child != nil {
			select {
			case <-child.done:
				return errors.New("ssh exited after the remote forward was rejected")
			default:
			}
		}
		if _, err := p.Run(sshShortStepTimeout, "test -S "+shellQuote(sock), nil, true); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the sandbox host never bound %s: %w", sock, last)
		}
		time.Sleep(callbackForwardConfirmPoll)
	}
}

// openTunnelForReattach opens the first transport generation with the persisted
// callback -R and only reports the callback as up (withCallback) once
// confirmCallbackForward saw sshd accept it (#5896). The data plane is the
// point of a reattach, and ssh's ExitOnForwardFailure makes a -R the sandbox
// host rejects fatal to the whole generation. So when the -R is rejected, or
// cannot be confirmed, it retries once WITHOUT it, returns why in note, and
// leaves the forward in place for the supervisor to re-establish on its next
// re-dial. The original error is the one returned when the data plane cannot
// open either way.
func (p *sandboxProvisioner) openTunnelForReattach() (localAddr string, withCallback bool, note string, err error) {
	localAddr, err = p.openTunnel(p.remoteAddr)
	if err == nil {
		if !p.callbackForwardPersisted() {
			return localAddr, false, "", nil
		}
		if cerr := confirmCallbackForward(p); cerr == nil {
			return localAddr, true, "", nil
		} else {
			err = cerr
		}
	}
	if !p.callbackForwardPersisted() {
		return "", false, "", err
	}
	note = "the callback forward could not be confirmed on reattach (" + err.Error() + "); it is retried each time the connection is re-dialed"
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
		return "", false, "", err
	}
	return localAddr, false, note, nil
}

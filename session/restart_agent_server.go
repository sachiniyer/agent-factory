package session

import (
	"errors"
	"fmt"
	"strings"
)

// Restarting a crashed agent-server (#5571 W1.4).
//
// The agent-server is detached (setsid), so it can die on its own — a crash, an
// OOM kill — while the host, the worktree and the tmux server the agent runs in
// all survive. Reattach used to give up there. It now relaunches the agent-server
// from the launch command the row persisted and adopts the surviving session
// through the ordinary restore path, so the agent is not re-run.
//
// Positive evidence only: the restart is licensed by a probe that proves the
// recorded PID is not this row's agent-server (gone, or reused by another
// program) on a host that answered. A host that did not answer, a probe that
// could not tell, or a live-but-unresponsive server are reported and left
// alone — starting a second agent-server beside a slow one would split the
// workspace between two owners.

const (
	agentServerProvedDead = "__af_agent_server_dead__"
	agentServerAliveMark  = "__af_agent_server_alive__"
	agentServerUnknown    = "__af_agent_server_unknown__"
)

// errAgentServerNotRestartable carries why a restart was not attempted.
var errAgentServerNotRestartable = errors.New("the agent-server was not restarted")

// remotePIDIdentityProbeScript answers, without touching the process, whether
// remotePID is still this row's agent-server. It reuses the identity prelude the
// reap's kill script runs, so the two can never disagree about what "ours" means.
func remotePIDIdentityProbeScript(remotePID, afPath, tokenRef string) string {
	return remotePIDIdentityFuncs(remotePID, afPath, tokenRef) +
		`if ! kill -0 "$pid" 2>/dev/null; then echo ` + agentServerProvedDead + `; exit 0; fi; ` +
		`matches_session; matched=$?; ` +
		`if [ "$matched" -eq 1 ]; then echo ` + agentServerProvedDead + `; ` +
		`elif [ "$matched" -eq 0 ]; then echo ` + agentServerAliveMark + `; ` +
		`else echo ` + agentServerUnknown + `; fi; exit 0`
}

// agentServerPIDName is the host-side record of the newest agent-server PID the
// restart launched. The launch wrapper writes it BEFORE af learns the PID, so a
// daemon that dies between the spawn and its own persist still leaves the host
// naming the server it started; the next proof checks it as well as the PID the
// row recorded.
const agentServerPIDName = "agent-server.pid"

func (p *sandboxProvisioner) agentServerPIDPath() string {
	return p.sessionDir + "/" + agentServerPIDName
}

// probeAgentServerPID returns nil only when the host proves pid is not this
// row's agent-server.
func (p *sandboxProvisioner) probeAgentServerPID(pid string) error {
	out, err := p.Run(sshShortStepTimeout,
		remotePIDIdentityProbeScript(pid, p.sessionDir+"/"+sshAfBinaryName, p.tokenRef), nil, false)
	if err != nil {
		return fmt.Errorf("%w: the host did not answer the identity probe: %v", errAgentServerNotRestartable, err)
	}
	switch got := strings.TrimSpace(string(out)); {
	case strings.HasSuffix(got, agentServerProvedDead):
		return nil
	case strings.HasSuffix(got, agentServerAliveMark):
		return fmt.Errorf("%w: its process (pid %s) is still running but did not answer", errAgentServerNotRestartable, pid)
	default:
		return fmt.Errorf("%w: the host could not tell whether pid %s is still the agent-server", errAgentServerNotRestartable, pid)
	}
}

// agentServerProvenDead reports whether the host proves that NO agent-server of
// this row is running: the PID the row recorded and the PID the host recorded at
// the last launch must each be proved dead. Any other answer is an error.
func (p *sandboxProvisioner) agentServerProvenDead() error {
	if p.remotePID == "" || !positivePID(p.remotePID) {
		return fmt.Errorf("%w: no recorded agent-server PID to prove dead", errAgentServerNotRestartable)
	}
	pids := []string{p.remotePID}
	out, err := p.Run(sshShortStepTimeout, "if [ -r "+shellQuote(p.agentServerPIDPath())+" ]; then cat "+shellQuote(p.agentServerPIDPath())+"; fi", nil, false)
	if err != nil {
		return fmt.Errorf("%w: the host did not answer the launch-record read: %v", errAgentServerNotRestartable, err)
	}
	if hostPID := strings.TrimSpace(string(out)); hostPID != "" {
		if !positivePID(hostPID) {
			return fmt.Errorf("%w: the host's launch record is unreadable", errAgentServerNotRestartable)
		}
		if hostPID != p.remotePID {
			pids = append(pids, hostPID)
		}
	}
	for _, pid := range pids {
		if err := p.probeAgentServerPID(pid); err != nil {
			return err
		}
	}
	return nil
}

// restartAgentServer relaunches the agent-server from the persisted launch
// command after proving the old one dead, and records its new PID and address.
// The token and callback credential travel in the session dir's 0600 env files
// the command sources, so the new process presents the same identity; expectedToken
// is checked against its banner so a restart can never bind to a stranger.
//
// persist makes the row's runtime identity durable. It runs twice: BEFORE the
// spawn, so the debt (adopt_pending) exists on disk before any process does, and
// again once the new PID and address are known. A failed write before the spawn
// aborts without one; a failed write after it stops the (still workspace-free)
// server rather than leave a process the row does not name.
func (p *sandboxProvisioner) restartAgentServer(expectedToken string, persist func() error) error {
	if p.launchScript == "" {
		return fmt.Errorf("%w: this row predates restartable launches", errAgentServerNotRestartable)
	}
	if err := p.agentServerProvenDead(); err != nil {
		return err
	}
	wasPending := p.adoptPending
	p.adoptPending = true
	if err := persist(); err != nil {
		p.adoptPending = wasPending
		return fmt.Errorf("%w: the restart could not be recorded before launching: %v", errAgentServerNotRestartable, err)
	}
	w := &sandboxWorkspace{shell: p, SessionDir: p.sessionDir}
	// The previous run's log is kept for diagnosis, and its banner removed so the
	// poll below cannot read the dead server's address. The wrapper records the
	// new PID on the host before reporting it.
	script := "mv -f " + shellQuote(w.LogPath()) + " " + shellQuote(w.LogPath()+".prev") + " 2>/dev/null; rm -f " +
		shellQuote(w.BannerPath()) + "; pid=$(" + p.launchScript + ") || exit 1; " +
		"printf '%s\n' \"$pid\" > " + shellQuote(p.agentServerPIDPath()) + "; echo \"$pid\""
	out, err := p.Run(sshShortStepTimeout, script, nil, false)
	if err != nil {
		return fmt.Errorf("restarting the af agent-server failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	pid := strings.TrimSpace(string(out))
	if !positivePID(pid) {
		return fmt.Errorf("restarting the af agent-server returned invalid background PID %q", pid)
	}
	p.remotePID = pid
	w.RemotePID = pid
	banner, err := w.readBanner(sshBannerPollTimeout, sshBannerPollInterval, sshShortStepTimeout)
	if err == nil && banner.Token != expectedToken {
		err = errors.New("the restarted af agent-server did not report this session's credential")
	}
	if err == nil {
		p.remoteAddr = banner.Addr
		if banner.Version != "" {
			p.agentServerVersion = banner.Version
		}
		if perr := persist(); perr != nil {
			err = fmt.Errorf("the restarted af agent-server's identity could not be recorded: %w", perr)
		}
	}
	if err != nil {
		// Nothing has asked this server to touch the workspace, so stopping it is
		// safe; leaving it would be a process the row does not name.
		p.stopUnadoptedServer()
		return err
	}
	return nil
}

// stopUnadoptedServer stops the restarted agent-server before it has been asked
// to adopt anything (best effort; the identity check keeps it off a reused PID).
// It must NOT be used once Provision has been sent: a server that holds the
// workspace tears it down on SIGTERM, and a half-adopted workspace is kept (and
// finished by the next reattach) rather than stopped.
func (p *sandboxProvisioner) stopUnadoptedServer() {
	_, _ = p.Run(sshShortStepTimeout, remotePIDIdentityKillScript(p.remotePID, p.sessionDir+"/"+sshAfBinaryName, p.tokenRef), nil, true)
}

// SetRuntimeIdentityPersist installs the durable write a W1.4 restart uses to
// record its runtime identity (see sandboxProvisioner.persistIdentity). A row
// without a restartable provisioner ignores it.
func (i *Instance) SetRuntimeIdentityPersist(persist func(*Instance) error) {
	if p := reattachProvisioner(i.currentBackend()); p != nil {
		i.mu.Lock()
		p.persistIdentity = persist
		i.mu.Unlock()
	}
}

// persistRuntimeIdentity copies the provisioner's live identity onto the row's
// cleanup handle and, when the daemon installed one, writes the row durably.
// Without a callback (tests, a build without a store) the in-memory handle is
// all there is, so an absent callback is not an error.
func (i *Instance) persistRuntimeIdentity(b Backend, p *sandboxProvisioner) error {
	i.mu.Lock()
	refreshRuntimeIdentity(b, p)
	persist := p.persistIdentity
	i.mu.Unlock()
	if persist == nil {
		return nil
	}
	return persist(i)
}

// noteAdoptedRestart settles what a restart-adoption means for the session's
// agent. Launch(false) either reattached the surviving agent or respawned it
// (tmux gone), and the launch response does not say which; survival cannot be
// proved, so the conservative verdict is used: the agent runtime counts as
// replaced, its idle evidence is cleared, and an in-flight task run ends
// interrupted. That only stops a replacement's first idle tick from completing
// the old run and firing on_complete. It runs BEFORE the endpoint is bound, so
// no observation of the new agent precedes it. The daemon stamps the owning
// task's status from ConsumeTaskRunInterrupted.
func (i *Instance) noteAdoptedRestart() {
	i.ClearIdleEvidence()
	i.markLoadRuntimeReplaced()
	i.markAgentRuntimeReplaced(true)
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.taskRunActive {
		i.endTaskRunInterruptedLocked()
		i.taskRunInterruptedStamp = true
	}
}

// retireLaunchScript forgets the stored launch command after the session's agent
// was swapped: the script still names the ORIGINAL agent, and restarting from it
// would attach a different program to the surviving tmux handle. A row without a
// script is reported as not restartable rather than restarted wrongly.
func (i *Instance) retireLaunchScript() {
	b := i.currentBackend()
	i.mu.Lock()
	defer i.mu.Unlock()
	switch be := b.(type) {
	case *sandboxBackend:
		if be.cleanup != nil {
			be.cleanup.LaunchScript = ""
		}
		if be.provisioner != nil {
			be.provisioner.launchScript = ""
		}
	case *sshBackend:
		if be.cleanup != nil {
			be.cleanup.LaunchScript = ""
		}
		if be.provisioner != nil {
			be.provisioner.launchScript = ""
		}
	}
}

// refreshRuntimeIdentity copies the provisioner's live identity onto the
// backend's persisted cleanup handle after a restart changed it. The caller
// holds i.mu, the lock ToInstanceData reads the handle under.
func refreshRuntimeIdentity(b Backend, p *sandboxProvisioner) {
	switch be := b.(type) {
	case *sandboxBackend:
		if be.cleanup != nil {
			be.cleanup.RemotePID, be.cleanup.RemoteAddr, be.cleanup.AgentServerVersion = p.remotePID, p.remoteAddr, p.agentServerVersion
			be.cleanup.AdoptPending = p.adoptPending
		}
	case *sshBackend:
		if be.cleanup != nil {
			be.cleanup.RemotePID, be.cleanup.RemoteAddr, be.cleanup.AgentServerVersion = p.remotePID, p.remoteAddr, p.agentServerVersion
			be.cleanup.AdoptPending = p.adoptPending
		}
	}
}

// nonAgentTabCount is how many shell and process tabs this (daemon-side) row
// records. The restarted agent-server cannot enumerate them, so the count travels
// with the adoption's Provision.
func (i *Instance) nonAgentTabCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	n := 0
	for idx, tab := range i.Tabs {
		if idx > 0 && tab != nil && tab.Kind != TabKindAgent {
			n++
		}
	}
	return n
}

// SetPersistedSiblingTabs records, on the agent-server's fresh instance, how many
// non-agent tabs the daemon's row says the session has (see adoptExistingWorktree).
func (i *Instance) SetPersistedSiblingTabs(n int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.persistedSiblingTabs = n
}

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

// agentServerProvedDead reports whether the host proves the recorded agent-server
// is not running. Any answer short of that is an error.
func (p *sandboxProvisioner) agentServerProvenDead() error {
	if p.remotePID == "" || !positivePID(p.remotePID) {
		return fmt.Errorf("%w: no recorded agent-server PID to prove dead", errAgentServerNotRestartable)
	}
	out, err := p.Run(sshShortStepTimeout,
		remotePIDIdentityProbeScript(p.remotePID, p.sessionDir+"/"+sshAfBinaryName, p.tokenRef), nil, false)
	if err != nil {
		return fmt.Errorf("%w: the host did not answer the identity probe: %v", errAgentServerNotRestartable, err)
	}
	switch got := strings.TrimSpace(string(out)); {
	case strings.HasSuffix(got, agentServerProvedDead):
		return nil
	case strings.HasSuffix(got, agentServerAliveMark):
		return fmt.Errorf("%w: its process (pid %s) is still running but did not answer", errAgentServerNotRestartable, p.remotePID)
	default:
		return fmt.Errorf("%w: the host could not tell whether pid %s is still the agent-server", errAgentServerNotRestartable, p.remotePID)
	}
}

// restartAgentServer relaunches the agent-server from the persisted launch
// command after proving the old one dead, and records its new PID and address.
// The token and callback credential travel in the session dir's 0600 env files
// the command sources, so the new process presents the same identity; expectedToken
// is checked against its banner so a restart can never bind to a stranger.
func (p *sandboxProvisioner) restartAgentServer(expectedToken string) error {
	if p.launchScript == "" {
		return fmt.Errorf("%w: this row predates restartable launches", errAgentServerNotRestartable)
	}
	if err := p.agentServerProvenDead(); err != nil {
		return err
	}
	w := &sandboxWorkspace{shell: p, SessionDir: p.sessionDir}
	// The previous run's log is kept for diagnosis, and its banner removed so the
	// poll below cannot read the dead server's address.
	script := "mv -f " + shellQuote(w.LogPath()) + " " + shellQuote(w.LogPath()+".prev") + " 2>/dev/null; rm -f " +
		shellQuote(w.BannerPath()) + "; " + p.launchScript
	out, err := p.Run(sshShortStepTimeout, script, nil, false)
	if err != nil {
		return fmt.Errorf("restarting the af agent-server failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	pid := strings.TrimSpace(string(out))
	if !positivePID(pid) {
		return fmt.Errorf("restarting the af agent-server returned invalid background PID %q", pid)
	}
	// Recorded before anything else can fail: the reap must know the live PID,
	// and a later reattach that finds this server responsive must know it has
	// not adopted the workspace yet.
	p.remotePID, p.adoptPending = pid, true
	w.RemotePID = pid
	// Up to here nothing has asked the new server to touch the workspace, so
	// stopping it is safe; a failure past the spawn must not leave it running
	// with only the in-memory PID updated (the caller persists the identity).
	banner, err := w.readBanner(sshBannerPollTimeout, sshBannerPollInterval, sshShortStepTimeout)
	if err == nil && banner.Token != expectedToken {
		err = errors.New("the restarted af agent-server did not report this session's credential")
	}
	if err != nil {
		p.stopUnadoptedServer()
		return err
	}
	p.remoteAddr = banner.Addr
	if banner.Version != "" {
		p.agentServerVersion = banner.Version
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

package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
)

// Restoring a preserved workspace (#5571 W8.6). An archive that did not push
// (W8.5) left the session directory on its host. Restore reattaches that
// workspace: it starts a new agent-server over the existing worktree and launches
// the agent with its recorded conversation. Nothing is cloned, fetched, hooked or
// reaped, so the tree, the index and the ignored files are exactly as archived.
//
// Re-provisioning happens only for a workspace PROVEN gone — the host answered
// that the session dir or its clone does not exist — and then onto the recorded
// branch or not at all. Any other uncertainty refuses and touches nothing.

// AdoptSpec names the preserved workspace a provision adopts.
type AdoptSpec struct {
	// SessionDir is the preserved session directory on the host.
	SessionDir string
	// Branch is the recorded branch; the adopted worktree must be on exactly it.
	Branch string
	// OldPID is the previous agent-server's pid, so a survivor is refused rather
	// than started beside.
	OldPID string
	// Transport is how the archived row reached its host, as it recorded it: the
	// composed ssh command (pinned address, host-key posture) and the config it came
	// from. Every host command of the adoption — the GONE probe first of all —
	// rides it, never the current host list or repo config.
	Transport *RecordedTransport

	// leaked is set by a failed provision that started a server it could not confirm
	// stopped; see abandonAdoption.
	leaked bool
}

// RecordedTransport is a preserved workspace's own route to its host.
type RecordedTransport struct {
	// SSHCmd is the recorded, address-pinned ssh invocation.
	SSHCmd string
	// Prepare readies the recorded host-key store (accept-new) before the first
	// command, as the reap does.
	Prepare func() error
	// SSH is the recorded ssh cleanup identity of the ssh backend, kept as the
	// adopted runtime's identity; nil for the sandbox backend.
	SSH *SSHRuntimeCleanupData
}

// recordedTransport reads the route to the host from the row's retained cleanup
// identity. A row without one cannot prove anything about its host.
func (i *Instance) recordedTransport() (*RecordedTransport, error) {
	p := i.sandboxProvisioner()
	if p == nil || strings.TrimSpace(p.sshCmd) == "" {
		return nil, fmt.Errorf("session %q recorded no transport to its host, so its preserved workspace cannot be checked; kill the session to remove it", i.Title)
	}
	t := &RecordedTransport{SSHCmd: p.sshCmd}
	if provider, ok := i.currentBackend().(runtimeCleanupProvider); ok {
		if data := provider.runtimeCleanupData(); data != nil && data.SSH != nil {
			c := *data.SSH
			cfg := c.Config
			cfg.Host, cfg.Port = normalizeLegacySSHAddress(cfg.Host, cfg.Port)
			posture := c.HostKeyVerification
			t.SSH = &c
			t.Prepare = func() error { return prepareSSHHostKeyStore(cfg, posture) }
		}
	}
	return t, nil
}

// ErrPreservedWorkspaceGone reports that the host positively answered that the
// preserved workspace no longer exists. It is the only verdict that permits
// replacing the workspace.
var ErrPreservedWorkspaceGone = errors.New("the preserved workspace is gone from its host")

// adoptCheckScript is the read-only proof. It prints exactly one verdict line:
//
//	GONE            the session dir or its clone does not exist
//	PRESENT <path>  the clone has a linked worktree on the recorded branch
//	RUNNING <pid>   the previous agent-server still runs for this dir
//	OTHER <reason>  anything else; the caller refuses
//
// It writes nothing. The pid check reads the process's command line, which holds
// the session dir through the af binary path, and matches both the recorded
// spelling and the canonical one (a symlinked home, /var vs /private/var): the
// running server's argv carries the physical path. A live pid whose command line
// cannot be read is refused, never taken for a stranger.
func adoptCheckScript(dir, branch, oldPID string) string {
	return fmt.Sprintf(`d=%s; b=%s; pid=%s
if [ ! -d "$d" ] || [ ! -e "$d/workspace" ]; then echo GONE; exit 0; fi
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  cmd=$(ps -ww -p "$pid" -o command= 2>/dev/null)
  canon=$(cd "$d" 2>/dev/null && pwd -P)
  if [ -z "$cmd" ]; then echo "OTHER cannot read the command line of live process $pid"; exit 0; fi
  case "$cmd" in *"$d"*) echo "RUNNING $pid"; exit 0;; esac
  if [ -n "$canon" ]; then case "$cmd" in *"$canon"*) echo "RUNNING $pid"; exit 0;; esac; fi
fi
if [ ! -e "$d/workspace/.git" ]; then echo "OTHER the preserved clone has no git repository"; exit 0; fi
wt=$(git -C "$d/workspace" worktree list --porcelain 2>/dev/null | awk -v b="refs/heads/$b" '/^worktree /{p=substr($0,10)} /^branch /{if($2==b){print p; exit}}')
if [ -z "$wt" ]; then echo "OTHER the clone has no linked worktree on branch $b"; exit 0; fi
if [ ! -d "$wt" ]; then echo "OTHER the worktree directory $wt is missing"; exit 0; fi
echo "PRESENT $wt"`, shellQuote(dir), shellQuote(branch), shellQuote(oldPID))
}

// parseAdoptVerdict reads the proof's output. gone is true only for an explicit
// GONE; every unrecognised or partial output is an error, never a gone.
func parseAdoptVerdict(out string) (worktree string, gone bool, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	switch {
	case last == "GONE":
		return "", true, nil
	case strings.HasPrefix(last, "PRESENT "):
		wt := strings.TrimSpace(strings.TrimPrefix(last, "PRESENT "))
		if wt == "" {
			return "", false, errors.New("the host reported a preserved worktree with no path")
		}
		return wt, false, nil
	case strings.HasPrefix(last, "RUNNING "):
		return "", false, fmt.Errorf("the previous agent-server (pid %s) is still running over the preserved workspace; not starting a second one beside it", strings.TrimSpace(strings.TrimPrefix(last, "RUNNING ")))
	case strings.HasPrefix(last, "OTHER "):
		return "", false, errors.New(strings.TrimSpace(strings.TrimPrefix(last, "OTHER ")))
	}
	return "", false, fmt.Errorf("the host's answer about the preserved workspace was not understood: %q", last)
}

// adoptWorkspace verifies the preserved workspace and returns the worktree to
// bind. A transport failure is an error, not a verdict: only the host's own GONE
// proves the workspace absent.
func (w *sandboxWorkspace) adoptWorkspace(timeout time.Duration) (string, error) {
	a := w.spec.Adopt
	out, err := w.shell.Run(timeout, adoptCheckScript(a.SessionDir, a.Branch, a.OldPID), nil, false)
	if err != nil {
		return "", fmt.Errorf("cannot confirm the preserved workspace at %s (the host could not be asked: %s): %w", a.SessionDir, strings.TrimSpace(string(out)), err)
	}
	wt, gone, err := parseAdoptVerdict(string(out))
	if gone {
		return "", fmt.Errorf("%w: %s", ErrPreservedWorkspaceGone, a.SessionDir)
	}
	if err != nil {
		return "", fmt.Errorf("the preserved workspace at %s cannot be reattached: %w", a.SessionDir, err)
	}
	return wt, nil
}

// provisionFailure is the cleanup after a failed provision. An ordinary provision
// owns the whole directory it made and reaps it; an adopting one owns nothing but
// the processes it started, so it stops those and leaves the workspace and its
// work untouched.
func (p *sandboxProvisioner) provisionFailure(err error) error {
	if p.spec.Adopt == nil {
		return p.reapProvisionFailure(err)
	}
	return p.abandonAdoption(err)
}

// abandonAdoption stops what an adopting provision started — the tunnel, the
// agent-server it launched and the account it synced — without removing anything
// the archive preserved.
func (p *sandboxProvisioner) abandonAdoption(cause error) error {
	p.stopTunnel()
	var cleanup []error
	if p.remotePID != "" && positivePID(p.remotePID) {
		kill := remotePIDIdentityKillScript(p.remotePID, p.sessionDir+"/"+sshAfBinaryName, p.tokenRef)
		if out, kerr := p.Run(sshShortStepTimeout, "( "+kill+" )", nil, true); kerr != nil {
			cleanup = append(cleanup, fmt.Errorf("stopping the agent-server it started: %s: %w", strings.TrimSpace(string(out)), kerr))
		}
	} else if p.launchAttempted {
		// A launch ran but no usable pid came back, so the server it started cannot
		// be targeted: that is unconfirmed, not clean.
		cleanup = append(cleanup, fmt.Errorf("the agent-server it started reported no usable pid (%q)", p.remotePID))
	}
	if p.account != nil {
		if aerr := p.account.teardownAccount(sshShortStepTimeout); aerr != nil {
			log.WarningLog.Printf("sandbox runtime: account teardown for %q: %v", p.sessionDir, aerr)
		}
	}
	if len(cleanup) > 0 {
		// af started a server it could not confirm stopped: its identity must reach
		// the row, not be dropped with this provisioner (leakedBackend).
		p.spec.Adopt.leaked = true
		return fmt.Errorf("%w (the preserved workspace at %s was not touched, but %w)", cause, p.sessionDir, errors.Join(cleanup...))
	}
	return fmt.Errorf("%w (the preserved workspace at %s was not touched)", cause, p.sessionDir)
}

// failedProvision is a failed provision's return: the cleanup first, then what it
// hands back (the error is computed before the result, which depends on it).
func (p *sandboxProvisioner) failedProvision(err error) (ProvisionResult, error) {
	ferr := p.provisionFailure(err)
	return p.adoptionFailureResult(), ferr
}

// adoptionFailureResult is what a failed provision hands back alongside its error:
// empty normally, but the NEW server's identity when an adopting provision started
// a server it could not confirm stopped. The row then records that identity
// (retainProvisionResultCleanup), so a restore checks the right pid and a kill or
// the orphan sweep can target the survivor, instead of the row keeping only the
// archived server's old identity.
func (p *sandboxProvisioner) adoptionFailureResult() ProvisionResult {
	if p.spec.Adopt == nil || !p.spec.Adopt.leaked {
		return ProvisionResult{}
	}
	return ProvisionResult{Backend: p.leakedBackend(), Teardown: p.reap}
}

// leakedBackend is the backend naming the server this adopting provision started.
func (p *sandboxProvisioner) leakedBackend() Backend {
	if p.label() == string(BackendSSH) {
		var c SSHRuntimeCleanupData
		if t := p.spec.Adopt.Transport; t != nil && t.SSH != nil {
			c = *t.SSH
		}
		c.SessionDir, c.RemotePID = p.sessionDir, p.remotePID
		c.RemoteAddr, c.AgentServerVersion, c.TokenRef = p.remoteAddr, p.agentServerVersion, p.tokenRef
		c.CallbackRef, c.CallbackReverse = p.callbackRef, p.callbackReverse()
		c.LaunchUnconfirmed = p.launchAttempted && !positivePID(p.remotePID)
		return &sshBackend{provisioner: p, cleanup: &c}
	}
	return &sandboxBackend{provisioner: p, cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: p.sshCmd, SessionDir: p.sessionDir, RemotePID: p.remotePID,
		RemoteAddr: p.remoteAddr, AgentServerVersion: p.agentServerVersion, TokenRef: p.tokenRef,
		CallbackRef: p.callbackRef, CallbackReverse: p.callbackReverse(),
		LaunchUnconfirmed: p.launchAttempted && !positivePID(p.remotePID),
	}}
}

// OwesPreservedWorkspace reports whether this row holds an unpushed preserved
// workspace. The automatic Lost-restore loop leaves such a row alone: a failed
// explicit restore of it is the operator's to retry, not something to re-probe on
// a timer.
func (i *Instance) OwesPreservedWorkspace() bool { return i.preservedWorkspaceOwed() != nil }

// preservedWorkspaceOwed reports whether this row holds an unpushed preserved
// workspace that a restore must reattach rather than replace.
func (i *Instance) preservedWorkspaceOwed() *ArchivedWorkspaceData {
	w := i.ArchivedWorkspace()
	if w == nil || w.Pushed || w.Dir == "" {
		return nil
	}
	return w
}

// preservedRemotePID is the previous agent-server's pid from the retained cleanup
// identity, or "".
func (i *Instance) preservedRemotePID() string {
	provider, ok := i.currentBackend().(runtimeCleanupProvider)
	if !ok {
		return ""
	}
	data := provider.runtimeCleanupData()
	switch {
	case data == nil:
		return ""
	case data.SSH != nil:
		return data.SSH.RemotePID
	case data.Sandbox != nil:
		return data.Sandbox.RemotePID
	}
	return ""
}

// restorePreservedWorkspace provisions the replacement runtime for a row whose
// archive kept its workspace: adopt it, or — only when the host proved it gone —
// replace it on the recorded branch.
func (i *Instance) restorePreservedWorkspace(w *ArchivedWorkspaceData) (*ArchivedWorkspaceData, error) {
	return i.restorePreservedWorkspaceWith(w, func(a *AdoptSpec) error {
		// A nil adopt here is the replacement of a workspace the host proved gone.
		return i.reprovisionRemoteOpts(a, a == nil)
	})
}

// restorePreservedWorkspaceWith is restorePreservedWorkspace with the provisioning
// injected, so the adopt-or-replace decision is testable without a host.
func (i *Instance) restorePreservedWorkspaceWith(w *ArchivedWorkspaceData, reprovision func(*AdoptSpec) error) (*ArchivedWorkspaceData, error) {
	transport, err := i.recordedTransport()
	if err != nil {
		return nil, err
	}
	err = reprovision(&AdoptSpec{SessionDir: w.Dir, Branch: i.Branch, OldPID: i.preservedRemotePID(), Transport: transport})
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, ErrPreservedWorkspaceGone) {
		return nil, err
	}
	// Proven gone: there is no work left to protect, so the ordinary path may
	// replace it. It pins the recorded branch, so it restores that branch or fails.
	if strings.TrimSpace(i.Branch) == "" {
		return nil, fmt.Errorf("cannot restore session %q: its preserved workspace is gone and no branch was recorded to restore from; nothing was changed. The work existed only in that workspace; to start over under this name, remove the row (af sessions kill %q) and create a new session: %w", i.Title, i.Title, err)
	}
	log.WarningLog.Printf("restore of %q: the preserved workspace %s is gone from its host; re-provisioning onto branch %q", i.Title, w.Dir, i.Branch)
	if err := reprovision(nil); err != nil {
		// The record stays: a retry probes the host again and reaches the same
		// verdict, instead of falling onto the ordinary path that may start a fresh
		// branch.
		return nil, err
	}
	i.mu.Lock()
	i.archivedWorkspace = nil
	i.touchLocked()
	i.mu.Unlock()
	return nil, nil
}

// rollbackAdoption undoes a reattach whose agent launch failed. The workspace is
// the archive's, not this attempt's: it is kept, the new agent-server and the
// account synced for it are stopped, and the row is put back as a preserved
// archive so a retry adopts it again and a kill still reaps it.
func (i *Instance) rollbackAdoption(w *ArchivedWorkspaceData) {
	var stopErr error
	if stopper, ok := i.AgentServer().(WorkspaceStopper); ok {
		stopErr = stopper.StopPreservingWorkspace()
	} else {
		stopErr = ErrPreserveUnsupported
	}
	if stopErr != nil {
		// The stop RPC did not confirm. Fall back to the identity kill, which acts
		// on the pid and token af recorded for this server; only if that cannot
		// confirm either is the server unaccounted for.
		stopErr = i.confirmServerStopped()
	}
	i.releaseSyncedAccount()
	i.stopRemoteTransport()
	i.revokeSandboxCredential()
	i.mu.Lock()
	i.agentSrv = nil
	i.remoteClient = nil
	i.archivedWorkspace = cloneArchivedWorkspace(w)
	if stopErr != nil {
		// af never forgets the identity of a server it started: the backend and
		// teardown installed by the adoption stay, carrying its pid and token ref,
		// and the row is marked unknown so that is persisted, visible to the orphan
		// sweep, and blocks any further adoption until confirmStoppedBeforeAdopt.
		i.runtimeCleanupStateUnknown = true
		log.WarningLog.Printf("adopt rollback for %q: the agent-server it started is not confirmed stopped (%v); its identity stays recorded and the next restore reaps it first", i.Title, stopErr)
	}
	// started is left exactly as the restore fence set it: AbortRestoreToLost does
	// not restore it, and both a manual and an automatic recovery refuse a row that
	// is not started, which would strand the preserved workspace with no retry.
	i.touchLocked()
	i.mu.Unlock()
}

// confirmServerStopped runs the identity-checked kill against the agent-server
// recorded in this row's cleanup identity, over its recorded transport. Success
// means the process is gone (or was never this session's); anything else is
// unconfirmed. With no recorded pid there is nothing to target, which is also
// unconfirmed when the row is marked unknown.
func (i *Instance) confirmServerStopped() error {
	p := i.sandboxProvisioner()
	if p == nil {
		return errors.New("no recorded transport to reach the host")
	}
	pid := p.remotePID
	if !positivePID(pid) {
		return fmt.Errorf("no usable pid recorded for the agent-server (%q)", pid)
	}
	if t, err := i.recordedTransport(); err == nil && t.Prepare != nil {
		if err := t.Prepare(); err != nil {
			return err
		}
	}
	kill := remotePIDIdentityKillScript(pid, p.sessionDir+"/"+sshAfBinaryName, p.tokenRef)
	if out, err := p.Run(sshShortStepTimeout, "( "+kill+" )", nil, true); err != nil {
		return fmt.Errorf("stopping agent-server %s: %s: %w", pid, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// confirmStoppedBeforeAdopt gates every further adoption on a row left unknown by
// an unconfirmed stop: the survivor is reaped first, and only a confirmed stop
// (or proof it is gone) clears the mark. Anything else refuses, so a second
// server is never started beside a survivor.
func (i *Instance) confirmStoppedBeforeAdopt() error {
	i.mu.RLock()
	unknown := i.runtimeCleanupStateUnknown
	i.mu.RUnlock()
	if !unknown {
		return nil
	}
	if err := i.confirmServerStopped(); err != nil {
		return fmt.Errorf("session %q: an agent-server af started in its preserved workspace is not confirmed stopped (%v); not starting another beside it — retry once the host is reachable, or kill the session", i.Title, err)
	}
	i.mu.Lock()
	i.runtimeCleanupStateUnknown = false
	i.touchLocked()
	i.mu.Unlock()
	return nil
}

// AdoptExistingWorktree binds this in-sandbox instance to a worktree that already
// exists on the host (#5571 W8.6), instead of creating one. The caller has proved
// the worktree is present and on the recorded branch; this only records it.
func (i *Instance) AdoptExistingWorktree(worktreePath string) error {
	i.mu.RLock()
	repo, title, branch := i.Path, i.Title, i.branchName
	i.mu.RUnlock()
	gw, err := git.NewGitWorktreeFromStorage(repo, worktreePath, title, branch, "", false, true)
	if err != nil {
		return fmt.Errorf("adopt worktree %s: %w", worktreePath, err)
	}
	i.mu.Lock()
	i.setGitWorktreeLocked(gw)
	if i.Branch != branch && branch != "" {
		i.Branch = branch
	}
	i.mu.Unlock()
	return nil
}

// LaunchAdopted starts the agent in the adopted worktree, resuming conv where
// this host's provider store holds it. It is the respawn a Lost session gets, so
// the program, flags and resume rewrite are the ones every other relaunch uses,
// and it creates nothing: no worktree, no hook.
func (i *Instance) LaunchAdopted(conv *AgentConversationData) error {
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return fmt.Errorf("adopted launch: session %q has no local runtime", i.Title)
	}
	return i.launchAdoptedWith(conv, func() error { return b.Respawn(i) })
}

// launchAdoptedWith is LaunchAdopted with the respawn injected. The agent-server
// built this instance fresh, so nothing has marked it started: a respawn does not
// (it assumes a started instance), and an unstarted instance refuses every prompt
// ("instance not started") even with its agent running. A launch that came up
// marks it started, exactly as a first-time launch does.
func (i *Instance) launchAdoptedWith(conv *AgentConversationData, respawn func() error) error {
	if conv != nil && conv.HasID() {
		if ConversationResumableHere(i.Program, i.GetWorktreePath(), *conv) {
			i.SetResumeConversation(*conv)
		} else {
			log.InfoLog.Printf("adopted launch: conversation %s is not in this host's %s store; starting a fresh conversation rather than resuming the latest one", conv.ID, conv.Agent)
		}
	}
	if err := respawn(); err != nil {
		return err
	}
	i.mu.Lock()
	i.markStartedLocked(true)
	i.mu.Unlock()
	return nil
}

// markStartedLocked is the one place a successful launch — first-time, restore or
// adopted — marks the instance started, so no launch path can forget it. touch
// bumps the update stamp when the flag changed. The caller holds i.mu.
func (i *Instance) markStartedLocked(touch bool) {
	if i.started {
		return
	}
	i.started = true
	if touch {
		i.touchLocked()
	}
}

// beginAdoption is provision()'s first step when adopting: the session dir is the
// archive's, verified read-only, and nothing in it is created or rewritten.
func (p *sandboxProvisioner) beginAdoption(w *sandboxWorkspace) error {
	w.SessionDir = p.spec.Adopt.SessionDir
	p.sessionDir = w.SessionDir
	wt, err := w.adoptWorkspace(sshShortStepTimeout)
	if err != nil {
		return err
	}
	w.adoptedWorktree = wt
	return nil
}

// clearStaleAdoptedCallbackSocket removes the previous run's callback socket from
// the preserved dir. A -R onto an existing socket fails the whole tunnel
// (ExitOnForwardFailure): the local port would answer the readiness probe and
// then vanish. The reattach after a daemon restart clears it for the same reason.
func (p *sandboxProvisioner) clearStaleAdoptedCallbackSocket() {
	if p.spec.Adopt != nil {
		p.clearStaleCallbackSocket()
	}
}

// awaitAdoptedServer gates an adopting launch on the agent-server answering
// through the forward. A listening local port only proves ssh bound it.
func (p *sandboxProvisioner) awaitAdoptedServer(localAddr, token string) error {
	if p.spec.Adopt == nil {
		return nil
	}
	if err := waitAdoptedServerReady(localAddr, token, p.spec.Title); err != nil {
		return p.sandboxErr(err)
	}
	return nil
}

// adoptReadyTimeout bounds how long an adopting launch waits for the new
// agent-server to answer through its tunnel; a var so a test can shorten it.
var adoptReadyTimeout = 20 * time.Second

// waitAdoptedServerReady polls the agent-server's health through the forward
// until it answers with the session's token. It is the readiness gate between
// "the tunnel is up" and the first agent RPC.
func waitAdoptedServerReady(localAddr, token, title string) error {
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: "http://" + localAddr, Token: token}, title)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(adoptReadyTimeout)
	for {
		_, err := rc.health()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the reattached agent-server did not answer through its tunnel within %s: %w", adoptReadyTimeout, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// adoptedTransportOr is the ssh invocation a provision uses: the archived row's
// recorded one when adopting, else the one the current configuration yields.
func adoptedTransportOr(spec ProvisionSpec, current string) string {
	if spec.Adopt != nil && spec.Adopt.Transport != nil {
		return strings.TrimSpace(spec.Adopt.Transport.SSHCmd)
	}
	return current
}

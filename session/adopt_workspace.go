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
// the session dir through the af binary path.
func adoptCheckScript(dir, branch, oldPID string) string {
	return fmt.Sprintf(`d=%s; b=%s; pid=%s
if [ ! -d "$d" ] || [ ! -e "$d/workspace" ]; then echo GONE; exit 0; fi
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  cmd=$(ps -ww -p "$pid" -o command= 2>/dev/null)
  case "$cmd" in *"$d"*) echo "RUNNING $pid"; exit 0;; esac
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
	}
	if p.account != nil {
		if aerr := p.account.teardownAccount(sshShortStepTimeout); aerr != nil {
			log.WarningLog.Printf("sandbox runtime: account teardown for %q: %v", p.sessionDir, aerr)
		}
	}
	if len(cleanup) > 0 {
		return fmt.Errorf("%w (the preserved workspace at %s was not touched, but %w)", cause, p.sessionDir, errors.Join(cleanup...))
	}
	return fmt.Errorf("%w (the preserved workspace at %s was not touched)", cause, p.sessionDir)
}

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
	return i.restorePreservedWorkspaceWith(w, i.reprovisionRemoteWith)
}

// restorePreservedWorkspaceWith is restorePreservedWorkspace with the provisioning
// injected, so the adopt-or-replace decision is testable without a host.
func (i *Instance) restorePreservedWorkspaceWith(w *ArchivedWorkspaceData, reprovision func(*AdoptSpec) error) (*ArchivedWorkspaceData, error) {
	err := reprovision(&AdoptSpec{SessionDir: w.Dir, Branch: i.Branch, OldPID: i.preservedRemotePID()})
	if err == nil {
		return w, nil
	}
	if !errors.Is(err, ErrPreservedWorkspaceGone) {
		return nil, err
	}
	// Proven gone: there is no work left to protect, so the ordinary path may
	// replace it. It pins the recorded branch, so it restores that branch or fails.
	if strings.TrimSpace(i.Branch) == "" {
		return nil, fmt.Errorf("cannot restore session %q: its preserved workspace is gone and no branch was recorded to restore from: %w", i.Title, err)
	}
	log.WarningLog.Printf("restore of %q: the preserved workspace %s is gone from its host; re-provisioning onto branch %q", i.Title, w.Dir, i.Branch)
	i.mu.Lock()
	i.archivedWorkspace = nil
	i.touchLocked()
	i.mu.Unlock()
	return nil, reprovision(nil)
}

// rollbackAdoption undoes a reattach whose agent launch failed. The workspace is
// the archive's, not this attempt's: it is kept, the new agent-server and the
// account synced for it are stopped, and the row is put back as a preserved
// archive so a retry adopts it again and a kill still reaps it.
func (i *Instance) rollbackAdoption(w *ArchivedWorkspaceData) {
	if stopper, ok := i.AgentServer().(WorkspaceStopper); ok {
		_ = stopper.StopPreservingWorkspace()
	}
	i.releaseSyncedAccount()
	i.stopRemoteTransport()
	i.revokeSandboxCredential()
	i.mu.Lock()
	i.agentSrv = nil
	i.remoteClient = nil
	i.archivedWorkspace = cloneArchivedWorkspace(w)
	i.started = false
	i.touchLocked()
	i.mu.Unlock()
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
	if conv != nil && conv.HasID() {
		if ConversationResumableHere(i.Program, i.GetWorktreePath(), *conv) {
			i.SetResumeConversation(*conv)
		} else {
			log.InfoLog.Printf("adopted launch: conversation %s is not in this host's %s store; resuming the latest instead", conv.ID, conv.Agent)
		}
	}
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return fmt.Errorf("adopted launch: session %q has no local runtime", i.Title)
	}
	return b.Respawn(i)
}

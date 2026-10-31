package session

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// Archive without publishing (#5571 W8.5). The default archive of an ssh/sandbox
// session stops the agent and leaves the workspace where it is, owned by the
// archived row — as a local archive keeps the worktree. Nothing is committed,
// pushed or removed; pushing is the explicit `push` option, which keeps
// ArchiveSandbox's snapshot-push-reap path. Restore from a preserved, unpushed
// workspace is W8.6; until then it is refused (RestoreRefusedForPreservedWorkspace).

// ArchivedWorkspaceData says where an archived off-box session's work lives.
type ArchivedWorkspaceData struct {
	// Host is the listed host (global `hosts`) the workspace is on, when one was
	// picked; empty for a session built from the repo's own ssh settings.
	Host string `json:"host,omitempty"`
	// Dir is the session directory on the host that holds the workspace. Empty
	// once the work was pushed and the sandbox reaped.
	Dir string `json:"dir,omitempty"`
	// Pushed is true when the branch was pushed to origin by this archive.
	Pushed bool `json:"pushed"`
	// Branch is the branch that was pushed; empty when Pushed is false.
	Branch string `json:"branch,omitempty"`
}

func cloneArchivedWorkspace(w *ArchivedWorkspaceData) *ArchivedWorkspaceData {
	if w == nil {
		return nil
	}
	c := *w
	return &c
}

// WorkspaceStopper is implemented by an AgentServer that can stop its agent and
// every tab while leaving the workspace untouched: no commit, no push, no
// removal. It is optional so a server predating the route simply cannot preserve.
type WorkspaceStopper interface {
	StopPreservingWorkspace() error
}

// ErrPreserveUnsupported means this session's agent-server cannot stop without
// removing the workspace; the caller archives with a push instead.
var ErrPreserveUnsupported = errors.New("this session's agent-server cannot stop without removing its workspace")

// ArchivedWorkspace returns where this archived session's work lives, or nil.
func (i *Instance) ArchivedWorkspace() *ArchivedWorkspaceData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneArchivedWorkspace(i.archivedWorkspace)
}

// preservedSessionDir is the host session directory af recorded for this
// session's runtime, or "" when the backend keeps no such directory (docker and
// hook have nothing af can leave in place and reattach).
func (i *Instance) preservedSessionDir() string {
	provider, ok := i.currentBackend().(runtimeCleanupProvider)
	if !ok {
		return ""
	}
	data := provider.runtimeCleanupData()
	switch {
	case data == nil:
		return ""
	case data.SSH != nil:
		return data.SSH.SessionDir
	case data.Sandbox != nil:
		return data.Sandbox.SessionDir
	}
	return ""
}

// CanPreserveWorkspace reports whether this off-box session keeps a session
// directory a no-push archive can leave in place.
func (i *Instance) CanPreserveWorkspace() bool {
	return strings.TrimSpace(i.preservedSessionDir()) != ""
}

// ArchiveSandboxPreserving archives an off-box session without publishing: it
// stops the agent, drops the daemon's connection and the session's callback
// credential, and leaves the workspace on its host. The runtime cleanup identity
// is kept, so killing the archived row later still reaps exactly what af made.
//
// The archive records Archived only over a stop it has proven. A row with no live
// client (a daemon restart, or a Lost row) is reattached first; when it cannot be
// reached the archive is refused, because a missing client does not mean the
// remote agent is gone.
func (i *Instance) ArchiveSandboxPreserving() (ArchivedWorkspaceData, error) {
	i.mu.RLock()
	isSandbox := i.capabilitiesLocked().Workspace == WorkspaceRemote
	hasClient := i.remoteClient != nil
	host := i.Host
	i.mu.RUnlock()
	if !isSandbox {
		return ArchivedWorkspaceData{}, fmt.Errorf("session %q is not a sandbox session; cannot archive via stop/preserve", i.Title)
	}
	dir := strings.TrimSpace(i.preservedSessionDir())
	if dir == "" {
		return ArchivedWorkspaceData{}, fmt.Errorf("session %q has no preservable session directory: %w", i.Title, ErrPreserveUnsupported)
	}
	if !hasClient {
		// A missing client is not evidence the agent stopped: a daemon restart
		// leaves the remote agent and its tabs running and rebuilds only the cleanup
		// handle. Reconnect from the persisted runtime identity and stop it for real;
		// if that cannot be done the archive is refused rather than recorded over a
		// live agent (#5868).
		if err := i.ReattachSandbox(); err != nil {
			i.mu.RLock()
			bound := i.remoteClient != nil
			i.mu.RUnlock()
			if !bound {
				return ArchivedWorkspaceData{}, fmt.Errorf("session %q: cannot confirm its agent has stopped, because the daemon has no connection to it and reconnecting failed: %w; retry once the host is reachable, or kill the session to remove it", i.Title, err)
			}
		}
	}
	stopper, ok := i.AgentServer().(WorkspaceStopper)
	if !ok {
		return ArchivedWorkspaceData{}, fmt.Errorf("session %q: %w", i.Title, ErrPreserveUnsupported)
	}
	if err := stopper.StopPreservingWorkspace(); err != nil {
		return ArchivedWorkspaceData{}, fmt.Errorf("failed to stop session %q before archive: %w", i.Title, err)
	}
	loc := ArchivedWorkspaceData{Host: host, Dir: dir}
	// The agent is stopped, so nothing can refresh the synced account any more:
	// write it back, then remove it from the host. The skipped teardown is what
	// normally does this (the reap), and an archive that keeps the workspace must
	// not keep the credentials with it (W8.5: "synced credentials are removed").
	i.releaseSyncedAccount()
	// The skipped teardown is what normally stops the ssh transport; without this
	// each archived session would keep its ssh process, forwarded port and
	// supervisor (re-dialling if the link drops) until the daemon exits.
	i.stopRemoteTransport()
	// The callback credential was minted for the runtime that just stopped.
	i.revokeSandboxCredential()
	i.mu.Lock()
	i.agentSrv = nil
	i.remoteClient = nil
	// runtimeTeardown and the backend's cleanup identity stay: they are what lets a
	// later kill or prune of this archived row reap the preserved directory.
	i.archivedWorkspace = cloneArchivedWorkspace(&loc)
	i.started = false
	i.touchLocked()
	i.mu.Unlock()
	return loc, nil
}

// sandboxProvisioner is this session's provisioner, or nil for a backend with none.
func (i *Instance) sandboxProvisioner() *sandboxProvisioner {
	switch b := i.currentBackend().(type) {
	case *sandboxBackend:
		return b.provisioner
	case *sshBackend:
		return b.provisioner
	}
	return nil
}

// releaseSyncedAccount writes the session's synced account back to the registry
// and removes it from the host (W7.3's teardownAccount), for a path that keeps the
// workspace but must not keep the credentials. Best-effort and logged, exactly like
// the reap path: a failed write-back never blocks the archive, and the reap of the
// archived row later removes the session directory — credentials included — as the
// backstop. A session with no synced account does nothing.
func (i *Instance) releaseSyncedAccount() {
	p := i.sandboxProvisioner()
	if p == nil {
		return
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if p.account == nil {
		return
	}
	if err := p.account.teardownAccount(sshShortStepTimeout); err != nil {
		log.WarningLog.Printf("archive of %q: removing the synced account from %q: %v", i.Title, p.sessionDir, err)
	}
}

// stopRemoteTransport stops the session's ssh tunnel and its supervisor without
// reaping anything on the host. The provisioner's cleanup identity is untouched,
// so a later kill of the archived row still reaps the directory (stopTunnel is
// idempotent, which reap relies on for retries).
func (i *Instance) stopRemoteTransport() {
	p := i.sandboxProvisioner()
	if p == nil {
		return
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	p.stopTunnel()
}

// keepsPreservedWorkspaceHandle reports whether an archived row still owes the
// reap of a workspace it left on its host: an unpushed preserve (#5571 W8.5). Such
// a row keeps its runtime-cleanup identity in storage, so a kill after a daemon
// restart removes exactly what af made instead of orphaning it (#5867). A pushed
// archive reaped its sandbox, so it owes nothing.
func (d InstanceData) keepsPreservedWorkspaceHandle() bool {
	w := d.ArchivedWorkspace
	return isSandboxBackendType(d.BackendType) && w != nil && !w.Pushed && w.Dir != ""
}

package session

import (
	"errors"
	"fmt"
	"strings"
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
	i.mu.RLock()
	defer i.mu.RUnlock()
	provider, ok := i.backend.(runtimeCleanupProvider)
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
// A row with no live client (Lost, or unreachable and since re-dialled out) has
// no agent af can still reach; it archives too, since the workspace is on the host
// either way.
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
	if hasClient {
		stopper, ok := i.AgentServer().(WorkspaceStopper)
		if !ok {
			return ArchivedWorkspaceData{}, fmt.Errorf("session %q: %w", i.Title, ErrPreserveUnsupported)
		}
		if err := stopper.StopPreservingWorkspace(); err != nil {
			return ArchivedWorkspaceData{}, fmt.Errorf("failed to stop session %q before archive: %w", i.Title, err)
		}
	}
	loc := ArchivedWorkspaceData{Host: host, Dir: dir}
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

// RestoreRefusedForPreservedWorkspace explains why a restore must not re-provision
// an archived row whose work was preserved on its host and never pushed: a fresh
// sandbox would clone a branch origin does not have. Reattaching the preserved
// workspace is W8.6; until then the way back is an archive with a push.
func (i *Instance) RestoreRefusedForPreservedWorkspace() error {
	w := i.ArchivedWorkspace()
	if w == nil || w.Pushed || w.Dir == "" {
		return nil
	}
	where := w.Dir
	if w.Host != "" {
		where = w.Host + ":" + w.Dir
	}
	return fmt.Errorf("session %q was archived without pushing: its work is preserved at %s, and restoring from a preserved workspace is not available yet (#5571 W8.6); the work is untouched, and you can push it from the host", i.Title, where)
}

package daemon

import (
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// runtimeNameNamespace is the extra (non-title) name a backend claims during
// create admission. The enum makes the alternatives exclusive: a create cannot
// accidentally be treated as both a local tmux runtime and a remote hook, which
// two independent booleans would permit.
type runtimeNameNamespace uint8

const (
	runtimeNamespaceSandbox runtimeNameNamespace = iota
	runtimeNamespaceLocalTmux
	runtimeNamespaceRemoteHook
)

func runtimeNamespaceForKind(kind session.BackendKind) runtimeNameNamespace {
	switch kind {
	case session.BackendLocal:
		return runtimeNamespaceLocalTmux
	case session.BackendHook:
		return runtimeNamespaceRemoteHook
	default:
		return runtimeNamespaceSandbox
	}
}

type titleConflictKind int

const (
	titleConflictNone titleConflictKind = iota
	titleConflictReserved
	titleConflictLive
	titleConflictDisk
)

type titleNamespace int

const (
	titleNamespaceNone titleNamespace = iota
	titleNamespaceBranch
	titleNamespaceTmux
)

// titleCollisionNamespace asks every name encoder a pair of local sessions will
// actually claim. Git and tmux intentionally have different grammars, so either
// one may collide while the other does not. The tmux half is enabled only when
// both records use the host-local runtime.
func (m *Manager) titleCollisionNamespace(repoPath, a, b string, bothUseLocalTmux bool) titleNamespace {
	if m.titlesCollide(a, b) {
		return titleNamespaceBranch
	}
	if bothUseLocalTmux && tmux.SanitizedNameForRepo(a, repoPath) == tmux.SanitizedNameForRepo(b, repoPath) {
		return titleNamespaceTmux
	}
	return titleNamespaceNone
}

// findTitleConflictLocked returns the existing title that conflicts with the
// given candidate, along with the source and namespace of the conflict. An empty
// result means the title is available. Local sessions claim both a git branch and
// a tmux session name; admission rejects either collision before creating a
// worktree or launching a runtime.
func (m *Manager) findTitleConflictLocked(repoID, repoPath, title string, localTmux bool, diskData []session.InstanceData) (string, titleConflictKind, titleNamespace) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			return existing, titleConflictReserved, titleNamespaceBranch
		}
	}
	if localTmux {
		nameKey := daemonInstanceKey(repoID, tmux.SanitizedNameForRepo(title, repoPath))
		if existing, reserved := m.reservedTmuxNames[nameKey]; reserved {
			return existing, titleConflictReserved, titleNamespaceTmux
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		bothUseLocalTmux := localTmux && inst.Capabilities().Workspace == session.WorkspaceLocalWorktree
		if namespace := m.titleCollisionNamespace(repoPath, inst.Title, title, bothUseLocalTmux); namespace != titleNamespaceNone {
			return inst.Title, titleConflictLive, namespace
		}
	}
	for _, data := range diskData {
		bothUseLocalTmux := localTmux && data.UsesLocalTmux()
		namespace := m.titleCollisionNamespace(repoPath, data.Title, title, bothUseLocalTmux)
		if namespace == titleNamespaceNone {
			continue
		}
		// Loading entries are transient TUI state with an empty worktree
		// path and cannot be restored. Older TUI binaries (#551) could
		// persist them to disk on quit, where they would block title
		// reuse forever. Treat them as ghosts that the next save will
		// reap rather than as live reservations.
		if data.Status == session.Loading {
			continue
		}
		return data.Title, titleConflictDisk, namespace
	}
	return "", titleConflictNone, titleNamespaceNone
}

// titlesCollide reports whether two session titles cannot coexist in the same
// repo because they would derive the same git branch. It delegates to the shared
// git.TitlesCollide helper so the daemon's authoritative validation and the
// TUI's naming pre-check stay in lockstep (#936).
func (m *Manager) titlesCollide(a, b string) bool {
	return git.TitlesCollide(a, b, m.cfg.BranchPrefix)
}

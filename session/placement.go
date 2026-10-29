package session

import (
	"path"
	"strings"
)

// Placement kinds name where a session runs.
const (
	PlacementLocal  = "local"
	PlacementRemote = "remote"
)

// PlacementData says where a session runs, for every surface that shows one
// (#5571 W6.4). It is derived on each snapshot, never persisted, and additive:
// clients that predate it read nothing and fall back to what they inferred
// from backend_type.
type PlacementData struct {
	// Kind is PlacementLocal for a git worktree on the daemon's machine and
	// PlacementRemote for a docker, ssh or sandbox runtime.
	Kind string `json:"kind"`
	// Host is the listed host (global `hosts`) the session was placed on, when one
	// was picked.
	Host string `json:"host,omitempty"`
	// Backend is the runtime type (docker, ssh, sandbox, remote); empty for local.
	Backend string `json:"backend,omitempty"`
	// Location is the workspace directory: the worktree path for a local session,
	// the workspace directory on the host for an ssh or sandbox one. Empty when
	// the runtime keeps none af can name (a docker container's filesystem).
	Location string `json:"location,omitempty"`
}

// Remote reports whether the session runs off the daemon's machine.
func (p *PlacementData) Remote() bool { return p != nil && p.Kind == PlacementRemote }

// Short is the one-word place for a narrow rail row: the host's name, else the
// runtime type, and "" for a local session (a local row says nothing extra).
func (p *PlacementData) Short() string {
	if !p.Remote() {
		return ""
	}
	if p.Host != "" {
		return p.Host
	}
	return p.Backend
}

// Detail is the full one-line place for a detail view: "local · <worktree>" or
// "<host> · <backend> · <workspace>".
func (p *PlacementData) Detail() string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.Remote() {
		parts = append(parts, p.Short())
		if p.Host != "" && p.Backend != "" {
			parts = append(parts, p.Backend)
		}
	} else {
		parts = append(parts, PlacementLocal)
	}
	if p.Location != "" {
		parts = append(parts, p.Location)
	}
	return strings.Join(parts, " · ")
}

func clonePlacement(p *PlacementData) *PlacementData {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// placementLocked derives the placement from the row's own state. Caller holds
// i.mu. backend is the runtime read under that lock (or via currentBackend),
// never the bare field. worktreePath is the local worktree path already
// projected into data.
func (i *Instance) placementLocked(backend Backend, backendType, worktreePath string) *PlacementData {
	if !isSandboxBackendType(backendType) && backendType != "remote" {
		return &PlacementData{Kind: PlacementLocal, Location: worktreePath}
	}
	p := &PlacementData{Kind: PlacementRemote, Host: i.Host, Backend: backendType}
	switch {
	case i.archivedWorkspace != nil && i.archivedWorkspace.Dir != "":
		p.Location = path.Join(i.archivedWorkspace.Dir, sshWorkspaceSubdir)
	default:
		if provider, ok := backend.(runtimeCleanupProvider); ok {
			if data := provider.runtimeCleanupData(); data != nil {
				switch {
				case data.SSH != nil && data.SSH.SessionDir != "":
					p.Location = path.Join(data.SSH.SessionDir, sshWorkspaceSubdir)
				case data.Sandbox != nil && data.Sandbox.SessionDir != "":
					p.Location = path.Join(data.Sandbox.SessionDir, sshWorkspaceSubdir)
				}
			}
		}
	}
	return p
}

// Placement is where this session runs. A row built from a daemon snapshot has
// no runtime of its own, so it returns the placement the daemon projected; the
// daemon's own rows derive it live.
func (i *Instance) Placement() *PlacementData {
	// currentBackend takes i.mu itself, so read it before the lock below rather
	// than nesting two read locks around a pending writer.
	backend := i.currentBackend()
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.placement != nil {
		return clonePlacement(i.placement)
	}
	bt := ""
	if backend != nil {
		bt = backend.Type()
	}
	wt := ""
	if i.gitWorktree != nil {
		wt = i.gitWorktree.GetWorktreePath()
	}
	return i.placementLocked(backend, bt, wt)
}

// ReconcilePlacementSnapshot adopts the placement a daemon snapshot projected
// onto a client row, reporting whether it changed. A nil placement (an older
// daemon) leaves the row's own derivation in force.
//
// "Changed" is judged against the placement the row currently shows, which for a
// row with nothing adopted yet is its own derivation: a snapshot that merely
// restates what the row already derives is not a change, and must not repaint.
func (i *Instance) ReconcilePlacementSnapshot(p *PlacementData) bool {
	if p == nil {
		return false
	}
	backend := i.currentBackend()
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.placement
	if current == nil {
		bt := ""
		if backend != nil {
			bt = backend.Type()
		}
		wt := ""
		if i.gitWorktree != nil {
			wt = i.gitWorktree.GetWorktreePath()
		}
		current = i.placementLocked(backend, bt, wt)
	}
	same := *current == *p
	i.placement = clonePlacement(p)
	if same {
		return false
	}
	i.touchLocked()
	return true
}

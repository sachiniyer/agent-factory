package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// RefreshWorktreeMissing re-probes the tracked worktree path and folds a
// conclusive answer into the worktree-missing flag (#5102). It reports the
// flag's resulting value and whether this call changed it, so the daemon poll
// persists and publishes only on a transition.
//
// Only a conclusive answer moves the flag. Present clears it — a worktree the
// user rebuilt by hand is deliverable again. Absent sets it. Unknown (a stat
// error other than ENOENT, a bounded-probe timeout, or an af-owned relocation
// in flight) leaves it exactly as it was: an unanswerable path that was last
// seen absent is still not somewhere a prompt can land, and one last seen
// present must not be flagged on a guess. The probe runs outside i.mu because
// it is a filesystem call, bounded or not.
//
// Only a local worktree is probed. An off-box session's workspace lives in its
// sandbox or remote host, so a local lstat of its recorded path answers nothing
// about it — the same reason ArchiveSession routes it away from the relocation
// path entirely.
func (i *Instance) RefreshWorktreeMissing() (missing bool, changed bool) {
	i.mu.RLock()
	gw := i.gitWorktree
	local := i.capabilitiesLocked().Workspace == WorkspaceLocalWorktree
	flag := i.worktreeMissing
	i.mu.RUnlock()
	if gw == nil || !local {
		return flag, false
	}
	presence := gw.ProbeWorktreePresence()
	i.mu.Lock()
	defer i.mu.Unlock()
	switch presence {
	case git.WorktreePresenceAbsent:
		if !i.worktreeMissing {
			i.worktreeMissing = true
			i.worktreeMissingReason = fmt.Sprintf(
				"tracked worktree path %s does not exist (deleted outside af)", gw.GetWorktreePath(),
			)
			i.touchLocked()
			changed = true
		}
	case git.WorktreePresencePresent:
		if i.worktreeMissing {
			i.worktreeMissing = false
			i.worktreeMissingReason = ""
			i.touchLocked()
			changed = true
		}
	}
	return i.worktreeMissing, changed
}

// WorktreeMissing reports the worktree-missing flag and its operator-facing
// reason. It reads the last established answer and never probes; callers that
// are about to act on the worktree use RefreshWorktreeMissing instead.
func (i *Instance) WorktreeMissing() (bool, string) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.worktreeMissing, i.worktreeMissingReason
}

// WorktreeMissingFlag is WorktreeMissing without the reason, for renderers that
// only choose a glyph.
func (i *Instance) WorktreeMissingFlag() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.worktreeMissing
}

// SetWorktreeMissing records a worktree as gone with reason. The archive route
// for a deleted worktree uses it to stamp the archived row with what it found,
// so restore knows it is rebuilding rather than moving.
func (i *Instance) SetWorktreeMissing(reason string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.worktreeMissing = true
	i.worktreeMissingReason = reason
	i.touchLocked()
}

// ClearWorktreeMissing drops the flag once af has re-aimed the record at a
// location it is about to rebuild. If the rebuild does not materialize the
// path, the next poll re-derives the flag.
func (i *Instance) ClearWorktreeMissing() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.worktreeMissing && i.worktreeMissingReason == "" {
		return
	}
	i.worktreeMissing = false
	i.worktreeMissingReason = ""
	i.touchLocked()
}

// ReconcileWorktreeMissing mirrors the daemon's worktree-missing flag onto an
// existing client projection, as ReconcileArchiveWarning does for the archive
// notice. Clients never probe the filesystem themselves — the daemon's answer is
// authoritative — so this only copies, and reports whether anything changed.
func (i *Instance) ReconcileWorktreeMissing(missing bool, reason string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.worktreeMissing == missing && i.worktreeMissingReason == reason {
		return false
	}
	i.worktreeMissing = missing
	i.worktreeMissingReason = reason
	return true
}

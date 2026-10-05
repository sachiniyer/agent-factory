package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session/git"
)

// RefreshWorktreeMissing re-probes the tracked worktree path and folds a
// conclusive answer into the worktree-missing flag (#5102). It reports the
// flag's resulting value and whether this call changed it, so the daemon poll
// persists and publishes only on a transition.
//
// The probe can only ever SET the flag. Absent sets it; Unknown (a stat error
// other than ENOENT, a bounded-probe timeout, or an af-owned relocation in
// flight) never moves it, so a path that cannot be answered is never flagged on
// a guess. Present does not clear it either, and that asymmetry is the point:
// once the directory was deleted, the agent's cwd is bound to the unlinked
// inode, and a pathname recreated by anything else — a mkdir, an unrelated `git
// worktree add` — leaves that pane exactly as undeliverable as before. The flag
// clears only through ClearWorktreeMissing, at the sites where af itself
// re-materializes or places the worktree: the respawn rebuild, restore, and an
// archive that moved or adopted it. The probe runs outside i.mu because it is a
// filesystem call, bounded or not; its answer is re-qualified under i.mu
// before it is written.
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
	if gw == nil || !local || flag {
		return flag, false
	}
	presence, probed := gw.ProbeWorktreePresenceAt()
	if presence != git.WorktreePresenceAbsent {
		return false, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	// The probe cannot see af's own ordinary archive or restore: those take a
	// record-free relocation claim that leaves the relocation snapshot clean, so
	// an lstat that lands mid-move reads ENOENT for af's own move. The operation
	// fence is what does see it. BeginArchive/BeginRestore set it before the move
	// can make the path absent, and both writes serialize on i.mu, so an op that
	// caused this ENOENT is visible here. An op that already finished re-aimed the
	// record, which the path comparison catches. Either way the answer is about a
	// path af owns, not one the user deleted (#5102).
	if i.inFlightOp != OpNone || i.gitWorktree != gw || gw.GetWorktreePath() != probed {
		return i.worktreeMissing, false
	}
	if !i.worktreeMissing {
		i.worktreeMissing = true
		i.worktreeMissingReason = worktreeMissingReasonForms[0].render(probed)
		i.touchLocked()
		changed = true
	}
	return true, changed
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

// ClearWorktreeMissing drops the flag. It is the only way the flag clears (the
// probe never does — see RefreshWorktreeMissing), so it is called only where af
// itself has just placed the worktree the record names: a respawn that rebuilt
// it and started a fresh pane there, every completed restore (moved back,
// adopted, or re-aimed for the respawn to rebuild), and an archive that moved or
// adopted it. At the archive and restore commits a set flag can only be stale —
// most plausibly a probe that raced af's own move. If a rebuild does not
// materialize the path, the next poll re-derives the flag.
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

// WorktreeMissingRemedy is the one operator instruction every surface gives for
// a worktree deleted outside af — send-prompt's refusal, `af sessions watch`,
// and the fleet watch — so they cannot drift into different advice (#5102).
//
// external selects the in-place (`--here`) shape. ArchiveSession refuses an
// external worktree outright — the checkout is the user's own, not af's to
// shelve — so recommending archive there would send the operator into a second
// refusal; kill is the only remedy af can carry out, and it removes nothing of
// the user's.
func WorktreeMissingRemedy(external bool) string {
	if external {
		return "remove it with 'af sessions kill' (an in-place session cannot be archived)"
	}
	return "archive it with 'af sessions archive' to keep its branch for a later restore, or remove it with 'af sessions kill'"
}

// worktreeMissingReasonForm is one af-authored MissingReason sentence: fixed
// prose around exactly one path. Every reason af writes comes from this table,
// which is what lets a bug report rewrite the path inside it and keep the
// sentence (RewriteWorktreeMissingReasonPath).
type worktreeMissingReasonForm struct{ prefix, suffix string }

func (f worktreeMissingReasonForm) render(path string) string { return f.prefix + path + f.suffix }

var worktreeMissingReasonForms = [...]worktreeMissingReasonForm{
	// Set by the probe on a live (or archived) row.
	{prefix: "tracked worktree path ", suffix: " does not exist (deleted outside af)"},
	// Stamped by the archive route that found the worktree already gone.
	{prefix: "worktree was already absent at ", suffix: " when archived (deleted outside af)"},
}

// WorktreeMissingArchivedReason is the reason an archive stamps on a row whose
// worktree it found already deleted at path.
func WorktreeMissingArchivedReason(path string) string {
	return worktreeMissingReasonForms[1].render(path)
}

// RewriteWorktreeMissingReasonPath rebuilds an af-authored MissingReason with
// its path passed through rewrite, keeping the prose. ok is false for text that
// is not one of af's forms — a record from a binary with different wording, or
// a hand edit — which a caller that must not leak it should drop whole.
func RewriteWorktreeMissingReasonPath(reason string, rewrite func(string) string) (string, bool) {
	for _, form := range worktreeMissingReasonForms {
		if len(reason) < len(form.prefix)+len(form.suffix) {
			continue
		}
		if strings.HasPrefix(reason, form.prefix) && strings.HasSuffix(reason, form.suffix) {
			path := reason[len(form.prefix) : len(reason)-len(form.suffix)]
			return form.render(rewrite(path)), true
		}
	}
	return "", false
}

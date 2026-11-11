package session

import (
	"strings"

	"github.com/sachiniyer/agent-factory/session/git"
)

// The setup-hook failure note: ONE field on the Instance, projected onto
// InstanceData and rendered on every surface that shows the session's
// warnings (#6667) — the same transport callbackUnavailable and
// agentConfigSkipped travel, for the same reason.
//
// A failing setup_commands entry does not block the session — a partially
// prepared workspace is still more useful than no session, matching the
// post_worktree_commands contract. What it must not be is silent: the note
// names the entry, the exit status or timeout, and the retained full-output
// log, and it persists on the row so the TUI, the web banner and the CLI all
// say "af setup hook failed: …" rather than leaving the operator to wonder
// why the workspace is missing its setup.
//
// SetupHookFailedPrefix is the exact prefix every surface renders before the
// note — the cross-surface contract, grep-able everywhere.
const SetupHookFailedPrefix = "af setup hook failed: "

// SetupHookFailed returns the recorded setup-hook failure note, or "" when
// the hook ran clean (or the session has none). Surfaces render it prefixed
// by SetupHookFailedPrefix.
func (i *Instance) SetupHookFailed() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.setupHookFailed
}

// noteSetupHookFailure appends one failure note to the record. A session can
// fail twice — the daemon-side hook and the in-worktree overlay — and both
// belong on the row.
func (i *Instance) noteSetupHookFailure(note string) {
	if strings.TrimSpace(note) == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.setupHookFailed == "" {
		i.setupHookFailed = note
	} else {
		i.setupHookFailed += "; " + note
	}
	i.touchLocked()
}

// noteWorktreeSetup records a worktree setup or rebuild's outcome on the row:
// every non-blocking failure the run produced (gw.SetupFailureNote — the
// setup payload's, and post_worktree_commands that did not run, #7079). It is
// the one path from a worktree run to the row, so a call site cannot surface
// half of it (#7084). A post_worktree_commands note an EARLIER run left is
// dropped first: the row describes this run, and a run that did start the
// commands must not keep claiming they did not.
func (i *Instance) noteWorktreeSetup(gw *git.GitWorktree) {
	if gw == nil {
		return
	}
	i.mu.Lock()
	if kept := dropSetupNotes(i.setupHookFailed, git.HookNotRunNotePrefix); kept != i.setupHookFailed {
		i.setupHookFailed = kept
		i.touchLocked()
	}
	i.mu.Unlock()
	i.noteSetupHookFailure(gw.SetupFailureNote())
}

// legacyHookNotRunTail is how the second half of the one not-run note an
// earlier build wrote with a "; " inside it begins (#7082, 9d9cdfa4ab): split
// on the row's separator, that note is a prefixed head and this tail.
const legacyHookNotRunTail = "run them in the worktree yourself, or recreate the session once it recovers: "

// dropSetupNotes removes the "; "-joined notes that begin with prefix. A hook
// not-run note never contains "; " itself (git.hookNotRunNote), so exactly that
// note goes, and every other note is kept as it was written. The earlier
// build's two-part note goes whole: its tail is dropped too, whether it still
// follows its head or a build since dropped the head alone and left the tail
// on the row — that text is unique to the old note (#7088, #7090 review).
func dropSetupNotes(notes, prefix string) string {
	if !strings.Contains(notes, prefix) && !strings.Contains(notes, legacyHookNotRunTail) {
		return notes
	}
	var kept []string
	for _, note := range strings.Split(notes, "; ") {
		if !strings.HasPrefix(note, prefix) && !strings.HasPrefix(note, legacyHookNotRunTail) {
			kept = append(kept, note)
		}
	}
	return strings.Join(kept, "; ")
}

// ReconcileSetupHookFailed mirrors the daemon's projected note onto an
// existing client projection. It is independent of liveness — a provisioning
// can rewrite it while the row stays Ready/Running — so, like
// ArchiveWarning, it reconciles on its own axis rather than folding into the
// status compare.
func (i *Instance) ReconcileSetupHookFailed(note string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.setupHookFailed == note {
		return false
	}
	i.setupHookFailed = note
	return true
}

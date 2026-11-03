package session

import "strings"

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

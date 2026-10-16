package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Retrying an unconfirmed tab teardown (#2669).
//
// CloseTab commits the shrunken roster before it kills tmux, so the kill can
// still time out (or answer while the session survives) after the tab is gone
// for good. tab_close.go records a TabCleanupData handle for exactly that case.
// This file is what eventually spends it: the daemon sweeps every restored
// instance once at startup, before readiness, which is the moment the previous
// daemon's unfinished kills are both discoverable and unraced.
//
// Startup is the ONLY retry trigger, deliberately. An immediate in-process retry
// would re-run the command that just timed out against the same wedged server,
// and a periodic one would keep doing so forever; neither adds information. The
// other half of the problem — a new tab deriving the survivor's name — is not
// solved by killing at all but by uniqueTabTmuxName reserving the pending token,
// so a spawn stays correct whether or not the retry has succeeded yet.

// PendingTabCleanup returns a copy of the instance's unconfirmed tab-teardown
// handles. Copied because the caller iterates outside i.mu while a concurrent
// close may append.
func (i *Instance) PendingTabCleanup() []TabCleanupData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if len(i.pendingTabCleanup) == 0 {
		return nil
	}
	return append([]TabCleanupData(nil), i.pendingTabCleanup...)
}

// RetryPendingTabCleanup re-attempts the tmux teardown of every tab whose close
// was durably committed but never confirmed, and retires each handle whose
// session is now positively gone. It reports how many handles were retired and
// how many remain unconfirmed.
//
// Retiring only on a CONFIRMED kill is the whole discipline. tmux Close is
// idempotent — killing an already-absent session succeeds (#967) — so a nil
// error genuinely means "no such session remains", which is the one answer that
// justifies dropping the last durable pointer to it. A timeout keeps the handle:
// an unanswered server is not evidence of absence, and #2669 is precisely the
// bug where an unknown outcome was read as a finished one.
//
// A zero retired count means no handle was retired and therefore nothing needs
// persisting, so the caller can skip the write entirely.
func (i *Instance) RetryPendingTabCleanup() (retired, remaining int) {
	for _, handle := range i.PendingTabCleanup() {
		if err := killTabCleanupSession(handle); err != nil {
			log.WarningLog.Printf(
				"session %q: tab cleanup for tmux session %q is still unconfirmed and will be retried on the next daemon start: %v",
				i.Title, handle.TmuxName, err)
			remaining++
			continue
		}
		i.mu.Lock()
		i.pendingTabCleanup = dropTabCleanup(i.pendingTabCleanup, &handle)
		i.touchLocked()
		i.mu.Unlock()
		log.InfoLog.Printf("session %q: reaped the leftover tmux session %q of a closed tab", i.Title, handle.TmuxName)
		retired++
	}
	return retired, remaining
}

// retainFailedTabCleanup records a cleanup handle for a just-spawned tab tmux
// session whose Start failed WITHOUT proving nothing is running. Start
// attaches ErrSessionNotStarted only when the pane is conclusively gone —
// every other failure (a proven-misplaced pane whose teardown is unconfirmed,
// a wedged probe, a post-spawn timeout) can leave a live or remain-on-exit
// pane holding this exact name. A caller that drops the session object on
// that error orphans the pane: invisible to later kill/archive teardown, and
// its name still blocks every spawn that would re-derive it. The handle puts
// the survivor on the same durable list the startup sweep retries by exact
// name and uniqueTabTmuxName already reserves against (#5174 review). It is
// in-memory like closeRemovedTabRetainingCleanup's handle — strictly better
// than dropping it, and the next successful persist carries it to disk.
//
// Fresh spawns only: a RESTORE path must not call this — the tab row itself
// still durably owns that name, and a stale handle would let a later sweep
// kill the session the tab successfully re-binds to (the same double-count
// the CloseTabByIDWithCommit rollback guards). Reattach survivors are the
// deferred quarantine half of that work (#5192).
//
// ErrSessionNameTaken wraps ErrSessionNotStarted deliberately, so a name
// legitimately held by a live session is never staged here — the sweep's
// exact-name kill would otherwise reap someone else's pane.
//
// The spawn-attempted boundary is the other half of the same protection: a
// Start that never reached new-session (a wedged has-session probe, a
// spawn-dir refusal, an env-preparation failure) cannot have left an af pane
// behind the name — but the failed probe may itself have hidden an existing
// untracked session under it. Retaining there would point the exact-name
// kill at a pane this attempt never created (#5174 review).
func (i *Instance) retainFailedTabCleanup(ts *tmux.TmuxSession, tmuxName string, startErr error) {
	if tmuxName == "" || startErr == nil || ts == nil || !ts.SpawnAttempted() ||
		errors.Is(startErr, tmux.ErrSessionNotStarted) {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, h := range i.pendingTabCleanup {
		if h.TmuxName == tmuxName {
			return
		}
	}
	i.pendingTabCleanup = append(i.pendingTabCleanup, TabCleanupData{TmuxName: tmuxName})
	i.touchLocked()
}

// killTabCleanupSession tears down one leftover session by its EXACT persisted
// name — the same exact-name binding restoreLocalTabs uses, never a name
// re-derived from a tab, which after a rename or a suffix walk would target the
// wrong session.
//
// It goes through the same package var restore does so tests can stay hermetic;
// production builds a real tmux session. Pane state is ignored for the reason
// closeRemovedTab ignores it: reaping one tab's session touches no worktree, so
// nothing destructive follows an unknown.
func killTabCleanupSession(handle TabCleanupData) error {
	if handle.TmuxName == "" {
		return fmt.Errorf("tab cleanup handle has no tmux session name")
	}
	// The program is irrelevant to a kill and this session is never started from
	// here, so an empty one keeps the handle honest about carrying no command.
	ts := restoreTmuxSession(handle.TmuxName, "")
	if _, err := ts.Close(); err != nil {
		return err
	}
	return nil
}

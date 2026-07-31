package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
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
		i.mu.Unlock()
		log.InfoLog.Printf("session %q: reaped the leftover tmux session %q of a closed tab", i.Title, handle.TmuxName)
		retired++
	}
	return retired, remaining
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

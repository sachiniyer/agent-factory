package session

import "github.com/sachiniyer/agent-factory/log"

// Retiring off-box rows whose runtime is gone (#5954).
//
// An off-box shell or process tab lives in the SANDBOX; the daemon's roster row
// is only a handle on it. Replacing the sandbox — archive then restore, a
// recovery, a re-provision — gives the session a fresh agent-server that has
// never heard of those tabs, while the daemon keeps advertising their rows, so
// preview and terminal subscriptions return ErrTabGone for as long as the row
// lives. The rule that closes the class, wherever the sandbox came from: a row
// the sandbox's own roster, read successfully, does not hold is not a running
// tab, and the daemon stops advertising it.
//
// The rows are retired rather than recreated. A shell tab recreated under its
// old id would be a fresh shell with none of its history, and a process tab must
// never be run a second time (a process tab's command runs once, at creation —
// docs/cli.md); archiving a local session already documents that shell/process
// tabs "do not come back" (docs/web.md). Retiring is the one outcome that is the
// same for both kinds and cannot re-execute anything.

// offBoxPTYRowsLocked snapshots the identity of every off-box shell/process row,
// to be taken BEFORE the sandbox's roster is read: a tab created after the read
// is simply absent from it, and must not be mistaken for a lost one. Caller holds
// i.mu.
func (i *Instance) offBoxPTYRowsLocked() map[string]string {
	rows := map[string]string{}
	for _, t := range i.Tabs {
		if t.Kind == TabKindShell || t.Kind == TabKindProcess {
			if i.isOffBoxTabLocked(t) {
				rows[t.ID] = t.gen
			}
		}
	}
	return rows
}

// retireLostOffBoxTabs drops each row in before that the sandbox's roster does
// not hold, or holds as a different incarnation. before is the snapshot taken
// ahead of the read, so a row created concurrently is never retired. Reports the
// ids dropped so the caller can end their streams outside the lock.
func (i *Instance) retireLostOffBoxTabs(before map[string]string, byID map[string]TabData) (retired []string) {
	if len(before) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, gen := range before {
		idx, ok := i.tabIndexByIDLocked(id)
		if !ok || idx == 0 {
			continue
		}
		tab := i.Tabs[idx]
		// The row must still be the one that was snapshotted: a close and
		// re-create under the same id in between is a different tab.
		if tab.gen != gen || !i.isOffBoxTabLocked(tab) {
			continue
		}
		if tab.restoreState == TabRestoreUnknown {
			// A restarted server we could not hand the tab back to does not hold
			// it, but its process may be alive: keep the row until the restore is
			// retried and answered. Only a confirmed-gone tab falls through to be
			// retired (#6237).
			continue
		}
		td, held := byID[id]
		if held && (gen == "" || td.Gen == "" || td.Gen == gen) {
			continue
		}
		log.WarningLog.Printf("session %q: sandbox no longer holds %s tab %q (%s); its row is removed because its terminal cannot come back",
			i.Title, TabKindName(tab.Kind), tab.Name, id)
		i.removeTabLocked(idx)
		retired = append(retired, id)
	}
	return retired
}

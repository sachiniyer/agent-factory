package session

import "fmt"

// Closing one tab: the destructive tab verb. Where tab_arrange.go's rename and
// reorder never touch tmux, a close ends a real process — and, since #1592 Phase
// 2 PR6, a real PTY STREAM. Both halves live here so neither can be updated
// without the other in view: killing the tmux session while leaving that tab's
// broker open is exactly the bug #2136 reported (a PTY-only subscriber blocked
// until its 15s keepalive gave up, with nothing on the wire to say the tab went
// away).

// CloseTab kills the tab at idx, ends its PTY stream, and removes it from Tabs.
// The agent tab (idx 0) is unclosable; CloseTab errors on idx 0 or any
// out-of-range index. The tab is removed from Tabs regardless of whether the tmux
// teardown succeeds (best-effort, matching LocalBackend.Kill) so a broken session
// can't wedge the tab list. Unlike Kill this does not wait for the pane to exit:
// the worktree is not being removed, so there is no #802 delete race to guard
// against.
func (i *Instance) CloseTab(idx int) error {
	i.mu.Lock()
	if idx <= 0 || idx >= len(i.Tabs) {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.removeTabLocked(idx)
	i.mu.Unlock()
	return i.closeRemovedTabRetainingCleanup(tab)
}

// CloseTabByID removes the tab with stable id, selecting and removing it in the
// same critical section. An ordinal resolved from an earlier snapshot is never
// applied to the live roster (#2200).
func (i *Instance) CloseTabByID(tabID string) error {
	i.mu.Lock()
	idx, exists := i.tabIndexByIDLocked(tabID)
	if !exists {
		i.mu.Unlock()
		return fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
	}
	if idx == 0 {
		i.mu.Unlock()
		return fmt.Errorf("tab cannot be closed")
	}
	tab := i.removeTabLocked(idx)
	i.mu.Unlock()
	return i.closeRemovedTabRetainingCleanup(tab)
}

// closeRemovedTabRetainingCleanup is closeRemovedTab for the two variants with
// no commit boundary of their own: it records a cleanup handle when teardown
// leaves the tmux session unaccounted for.
//
// These variants cannot pre-stage the handle the way CloseTabByIDWithCommit
// does, because they never persist — CreateTab's rollback reaches CloseTabByID
// precisely BECAUSE its write just failed, so no handle could reach disk at that
// moment anyway. Retaining it in memory is still strictly better than dropping
// it: this daemon stops re-deriving the surviving session's name for a new tab,
// and the next successful persist of the session carries the handle to disk
// where the startup sweep can spend it.
func (i *Instance) closeRemovedTabRetainingCleanup(tab *Tab) error {
	err := i.closeRemovedTab(tab)
	if err == nil {
		return nil
	}
	if handle, _ := stageTabCleanup(nil, tab); handle != nil {
		i.mu.Lock()
		i.pendingTabCleanup = append(i.pendingTabCleanup, *handle)
		i.mu.Unlock()
	}
	return err
}

// CommittedTabClose separates the durable roster decision from the best-effort
// runtime teardown that follows it. A nil TeardownErr confirms both the PTY
// stream and tmux close completed; a non-nil value means the tab is durably
// absent but runtime teardown could not be confirmed. Callers must not turn the
// latter into a failed commit and retry the state mutation as though it never
// happened.
//
// The unconfirmed case is not merely reported, it is RETAINED: the commit
// persists a TabCleanupData handle for the tab's tmux session, and only a
// confirmed teardown retires it. Settled is that retirement — the projection
// with the handle already dropped, which the caller should persist so the next
// daemon does not retry a kill that already succeeded. It is nil when there was
// nothing to retire (a tmux-less tab, or a teardown left unconfirmed), and
// persisting it is best-effort: losing that write only costs one idempotent
// re-kill of an absent session, whereas losing the handle itself is the leak
// this whole mechanism exists to prevent.
type CommittedTabClose struct {
	TeardownErr error
	Settled     *InstanceData
}

// CloseTabByIDWithCommit stages removal of the stable tab, calls commit with the
// resulting InstanceData, and only then performs irreversible stream/tmux
// teardown. If commit fails, the exact tab is restored at its original position
// before the error is returned, leaving both the roster and runtime available
// for a safe retry (#2669).
//
// The projection commit receives has the tab off Tabs AND its tmux identity on
// PendingTabCleanup, so the durable record never describes a state where the
// session is neither a tab nor a tracked cleanup. That handoff is the point: the
// roster shrinks irrevocably at the commit, but teardown runs afterwards and may
// time out, so something durable has to keep naming the tmux session until a
// kill is confirmed. A tombstone rather than a retained tab, because the tab is
// genuinely closed — it must not render, and a restore must not respawn it.
//
// Selection, staging, snapshotting, and commit run under the same instance lock.
// Besides keeping the rollback exact, this prevents another tab mutation from
// producing a projection between the staged roster and the one commit receives.
// commit must not call back into Instance: it receives the already-built
// projection for that reason.
func (i *Instance) CloseTabByIDWithCommit(
	tabID string,
	commit func(InstanceData) error,
) (CommittedTabClose, error) {
	i.mu.Lock()
	idx, exists := i.tabIndexByIDLocked(tabID)
	if !exists {
		i.mu.Unlock()
		return CommittedTabClose{}, fmt.Errorf("session %q tab id %q: %w", i.Title, tabID, ErrTabGone)
	}
	if idx == 0 {
		i.mu.Unlock()
		return CommittedTabClose{}, fmt.Errorf("tab cannot be closed")
	}

	tab := i.removeTabLocked(idx)
	handle, staged := stageTabCleanup(i.pendingTabCleanup, tab)
	i.pendingTabCleanup = staged
	data := i.toInstanceDataLocked()
	if err := commit(data); err != nil {
		// Nothing can change the roster while i.mu is held, so reinserting at idx
		// restores the precise pre-close order and the same live tab object. The
		// staged handle goes back too: the tab still owns its tmux session, so a
		// tombstone for it would double-count a live tab as awaiting cleanup and
		// could get its session killed under it by the startup sweep.
		i.Tabs = append(i.Tabs, nil)
		copy(i.Tabs[idx+1:], i.Tabs[idx:])
		i.Tabs[idx] = tab
		i.pendingTabCleanup = dropTabCleanup(i.pendingTabCleanup, handle)
		i.mu.Unlock()
		return CommittedTabClose{}, err
	}
	i.mu.Unlock()

	teardownErr := i.closeRemovedTab(tab)
	if teardownErr != nil || handle == nil {
		// Unconfirmed: the handle stays, and the daemon's startup sweep owns the
		// retry. Nothing to settle for a tmux-less tab either — it never staged one.
		return CommittedTabClose{TeardownErr: teardownErr}, nil
	}
	// Confirmed dead. Retire the handle and hand the caller the projection that
	// records the retirement.
	i.mu.Lock()
	i.pendingTabCleanup = dropTabCleanup(i.pendingTabCleanup, handle)
	settled := i.toInstanceDataLocked()
	i.mu.Unlock()
	return CommittedTabClose{Settled: &settled}, nil
}

// stageTabCleanup returns the cleanup handle a just-removed tab needs, plus the
// pending list with it appended. The handle is nil — and the list unchanged —
// for a tab with no tmux session of its own (web, VS Code, or never started):
// there is no process to leak and no token a later spawn could collide with, so
// recording one would only manufacture a retry against a name that never
// existed.
func stageTabCleanup(pending []TabCleanupData, tab *Tab) (*TabCleanupData, []TabCleanupData) {
	if tab == nil || tab.tmux == nil {
		return nil, pending
	}
	name := tab.tmux.SanitizedName()
	if name == "" {
		return nil, pending
	}
	handle := TabCleanupData{TabID: tab.ID, TmuxName: name}
	return &handle, append(pending, handle)
}

// dropTabCleanup removes handle from pending, matching on the tmux name rather
// than the TabID. The name is what a retry actually targets, and it is the field
// guaranteed unique across the list: uniqueTabTmuxName reserves pending tokens,
// so no live tab and no other handle can hold the same session name. TabID is
// carried for diagnostics and can be empty on a hand-edited or pre-#1738 record,
// which would leave such an entry unretirable. Returns nil for an emptied list
// so the projection omits the field entirely rather than persisting an empty
// array.
func dropTabCleanup(pending []TabCleanupData, handle *TabCleanupData) []TabCleanupData {
	if handle == nil || len(pending) == 0 {
		return pending
	}
	// Build a fresh slice rather than filtering in place: the staged list can share
	// a backing array with the caller's, and a projection built between the two
	// points holds its own copy only because it was copied eagerly. Rewriting
	// shared storage here would be a silent aliasing bug for one allocation saved.
	var kept []TabCleanupData
	for _, entry := range pending {
		if entry.TmuxName == handle.TmuxName {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// restoredTabCleanup rebuilds the pending-cleanup list from a persisted record,
// dropping entries with no tmux name. Such an entry cannot be targeted by a
// retry or reserved against by a spawn, so keeping it would only make the
// session look permanently un-swept.
func restoredTabCleanup(data []TabCleanupData) []TabCleanupData {
	var pending []TabCleanupData
	for _, entry := range data {
		if entry.TmuxName == "" {
			continue
		}
		pending = append(pending, entry)
	}
	return pending
}

// removeTabLocked detaches idx from the roster and returns the exact tab object
// whose stream/process teardown must continue after i.mu is released.
func (i *Instance) removeTabLocked(idx int) *Tab {
	tab := i.Tabs[idx]
	i.Tabs = append(i.Tabs[:idx], i.Tabs[idx+1:]...)
	return tab
}

// closeRemovedTab finishes the irreversible half outside i.mu: stream shutdown
// and tmux teardown may block and both can acquire their own locks.
func (i *Instance) closeRemovedTab(tab *Tab) error {
	// End this tab's stream BEFORE its tmux dies — the same order localAgentServer
	// .Kill uses for the session-wide case (brokers first, then the backend), and
	// for the same reason: a subscriber should be told the stream is over rather
	// than left reading a pane that has just been killed out from under it. Run
	// unconditionally, including on the tmux-less paths below, so the notification
	// never depends on how far the teardown gets.
	i.endTabPTYStream(tab.ID)

	if tab.tmux == nil {
		return nil
	}
	// Pane state deliberately ignored: closing ONE tab touches no worktree, so
	// nothing destructive follows an unknown here. (A Close that fails after the
	// tab was already dropped from i.Tabs above leaks the tmux session — true of
	// every Close failure, not just a timeout, and unchanged by #1917.)
	if _, err := tab.tmux.Close(); err != nil {
		return fmt.Errorf("failed to close tab %q: %w", tab.Name, err)
	}
	return nil
}

// endTabPTYStream shuts down the PTY broker serving the tab with this STABLE id
// (#1738), so its subscribers get a prompt end-of-stream (ErrTabClosed → an exit
// with reason "tab_closed" on the wire) instead of a silent socket that only the
// keepalive eventually reaps (#2136). Sibling tabs keep their own brokers and
// keep streaming.
//
// It reads the CACHED agent-server rather than calling AgentServer(): a session
// nobody has streamed has no cached server and therefore no brokers, so there is
// nothing to notify — and building one here purely to close nothing would be a
// side effect on a teardown path. The cached field is read under i.mu, then
// released before the call: closing a broker takes the agent-server's own lock,
// and the ordering rule is i.mu THEN s.mu, never nested the other way.
func (i *Instance) endTabPTYStream(tabID string) {
	i.mu.RLock()
	as := i.agentSrv
	i.mu.RUnlock()
	if c, ok := as.(tabStreamCloser); ok {
		c.closeTabStream(tabID)
	}
}

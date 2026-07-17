package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/ui/tree"
)

// The per-instance tab hotkeys — new (t), close (w), and the 1-9 jump — split
// out of handle_actions.go to keep that file under the length limit (#1145).
// They share the pane layer's stable-id + focused-pane rules (#1884/#1885/#1886).

// handleNewTab spawns a new shell tab in the selected instance and selects it
// (#930 PR 4). Single keypress, no prompt: the tab runs $SHELL in the instance's
// worktree. Remote instances have no local worktree and the hook protocol has no
// run-arbitrary-command verb, so new-tab is unsupported there: a remote session's
// only terminal tab is the one derived from remote_hooks.terminal_cmd (#930 PR 6).
//
// The spawn+persist is routed through the daemon's CreateTab RPC (#960): the
// daemon — the single writer — owns the new tab so its authoritative view holds
// it and the TUI no longer originates a tab write at all (#959). The TUI reflects
// the daemon-created tab locally via AttachShellTab for instant display (it
// reconnects to the session the daemon spawned, never a second colliding spawn);
// the snapshot reconcile (PR 3) keeps it mirrored thereafter. The daemon's soft
// cap (max tabs) error is surfaced verbatim.
func (m *home) handleNewTab() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.HasInFlightOp() {
		return m, nil
	}
	if !selected.Capabilities().TabManagement {
		return m, m.handleError(fmt.Errorf("only local sessions support new tabs — this session's workspace runs off-box (docker/ssh/remote), so there is no local worktree to spawn a tab in"))
	}

	name, err := createShellTabThroughDaemon(selected.Title, m.repoID)
	if err != nil {
		return m, m.handleError(err)
	}
	// The daemon spawned and persisted the tab; reflect it locally for instant
	// display without a second spawn. The daemon write is authoritative, so the
	// TUI never saves (#960 PR 4).
	if _, attachErr := selected.AttachShellTab(name); attachErr != nil {
		return m, m.handleError(attachErr)
	}

	// Select the fresh tab in the tree and open it as a pane (#1088): the
	// pre-N-pane behavior showed the new tab in the workspace immediately,
	// and the issue's canonical flow — agent pane + terminal pane side by
	// side for one instance — is exactly `s` then `t`.
	newIdx := len(tree.TabLabels(selected)) - 1
	m.store.SetActiveTab(newIdx)
	m.menu.SetActiveTab(newIdx)
	m.sidebar.SyncCursorToActiveTab()
	return m.openOrFocusPane(selected, newIdx)
}

// handleCloseTab closes the tab the user is LOOKING at and selects the previous
// (left) tab (#930 PR 4). When a pane holds focus, that target is the FOCUSED
// PANE's tab — resolved by the pane's own binding, not the tree's active tab
// (#1884): a pane-focused `1`-`9` jump retargets only the pane, so reading
// store.ActiveTab() here closed the tree's tab (a nonsense agent-tab error, or —
// worse — silently destroyed a DIFFERENT tab than the one on screen). With tree
// focus the target is the sidebar selection's active tab, unchanged.
//
// The agent tab (index 0) is unclosable — w on it is a gentle no-op message
// pointing at D for killing the whole session. A remote instance's tabs (agent +
// optional terminal_cmd terminal) are fixed by its hook config, not
// user-managed, so closing any of them is refused.
//
// The kill+persist is routed through the daemon's CloseTab RPC (#960): the
// daemon — the single writer — kills the tab's tmux and persists the shrunk
// list, so the TUI no longer originates a tab write at all (#959). The agent-tab
// and remote rules are still enforced TUI-side so the friendly message shows
// without a round-trip (the RPC enforces them too). The TUI drops the now-dead
// tab locally via DropClosedTab — a no-kill removal, since the daemon already
// tore the tmux session down.
func (m *home) handleCloseTab() (tea.Model, tea.Cmd) {
	// The focused pane owns the close target: w acts on the tab on screen, not
	// the tree cursor's tab (#1884). Fall back to the tree's selection + its
	// active tab when focus is on the tree.
	//
	// Both halves read ONE source of truth for their target, because every
	// wrong-target bug in this verb has been two sources disagreeing about which
	// tab is on screen:
	//   - the pane branch reads the EFFECTIVE binding, not p.Tab(): a previewing
	//     pane renders the preview target while p.Tab() reports the committed tab
	//     underneath, so the raw binding names a tab the user cannot see.
	//   - the tree branch reads the STORE's selection, not the sidebar cursor's:
	//     idx is store.ActiveTab(), which is by definition the active tab OF
	//     store.GetSelectedInstance(). The sidebar cursor is a DIFFERENT thing —
	//     it goes nil on a section header (where the store's display selection is
	//     deliberately sticky) and resolves to the ARCHIVED instance on an
	//     archived row (which never becomes the store's selection). Pairing this
	//     idx with that instance is a wrong-target waiting to happen.
	inst := m.store.GetSelectedInstance()
	idx := m.store.ActiveTab()
	if p := m.focusedOpenPane(); p != nil {
		b := m.effectivePaneBinding(p)
		inst, idx = b.instance, b.tab
		// The tab being previewed is about to be destroyed, so the transient
		// preview must not survive pointing at a dead slot — drop it and let the
		// pane fall back to its committed binding. Cheap even when a guard below
		// refuses the close: a preview is re-derived from the tree cursor on the
		// next tick.
		m.cancelPanePreview(false)
	}
	if inst == nil {
		return m, nil
	}
	if idx <= 0 {
		return m, m.handleError(fmt.Errorf("the agent tab can't be closed; use D to kill the session"))
	}
	if !inst.Capabilities().TabManagement {
		return m, m.handleError(fmt.Errorf("this session's tab list is fixed by its runtime and can't be edited"))
	}
	tabs := inst.GetTabs()
	if idx >= len(tabs) {
		return m, m.handleError(fmt.Errorf("tab cannot be closed"))
	}
	tabName := tabs[idx].Name
	// Capture the slot→identity list before the drop: reconcilePanesForTabs maps
	// the open panes' bindings across the change by stable tab id (#1088/#1886).
	oldKeys := paneTabKeys(inst)
	// Remember which tab the TREE is on, by identity. A pane-focused close can
	// remove a tab that is NOT the tree's active tab, and the tree must keep
	// pointing at its own tab across the shift (#1884 follow-up) — resolving it by
	// id afterwards is what keeps the arithmetic from guessing.
	// The ordinal rides along with the id: a freshly-created tab is ID-LESS by
	// design (AttachShellTab leaves Tab.ID empty until the next snapshot backfills
	// the daemon's), so there is a real window where the tree's tab cannot be found
	// by id at all and the ordinal is the only key it has.
	treeActiveID := ""
	treeActiveIdx := -1
	// Whose tab is store.ActiveTab()? The STORE's selected instance's — so that is
	// what decides whether the tree's active tab needs preserving across this
	// close, not the sidebar cursor. The cursor is nil on a section header and
	// points at the archived instance on an archived row, while the store's
	// display selection stays deliberately sticky on the live instance; reading
	// the cursor there made this false and left ActiveTab un-shifted, so returning
	// to the tree landed on a different surviving tab.
	treeIsSelected := inst == m.store.GetSelectedInstance()
	if treeIsSelected {
		if a := m.store.ActiveTab(); a >= 0 && a < len(tabs) {
			treeActiveIdx = a
			treeActiveID = tabs[a].ID
		}
	}

	if err := closeTabThroughDaemon(inst.Title, m.repoID, tabName); err != nil {
		return m, m.handleError(err)
	}
	// The daemon killed the tmux and persisted the shrunk list; drop the
	// now-dead tab locally without re-killing. The daemon write is
	// authoritative, so the TUI never saves (#960 PR 4).
	if dropErr := inst.DropClosedTab(idx); dropErr != nil {
		return m, m.handleError(dropErr)
	}

	// The kill shifts every higher tab slot down by one, so the open panes
	// bound to this instance must follow (#1088): the killed tab's pane
	// leaves the workspace, higher-slot panes re-bind so they keep showing
	// the same tab. Shared with the daemon-snapshot reconcile, which applies
	// the identical semantics when a tab disappears out-of-band.
	if m.reconcilePanesForTabs(inst, oldKeys, sameSessionTabs) {
		m.relayout()
	}

	// Re-point the tree at the tab it was already on, found by its id in the
	// shrunk roster — so a pane-focused close of a DIFFERENT tab leaves the tree's
	// tab alone (closing tab 3 must not move a tree sitting on tab 1), and a close
	// below it still follows the shift. Only when the tree's own tab is the one
	// that died does it fall back to the left/previous neighbour (#930 PR 4).
	if treeIsSelected {
		// Adjust the tree's OWN ordinal for the closed slot first. This is the answer
		// during the id-less window, and it is why `idx - 1` cannot be the fallback:
		// idx is the FOCUSED PANE's closed tab, so on a pane-focused close of some
		// other tab it names a neighbour of the wrong tab entirely — the tree would
		// jump even though its tab merely shifted (or did not move at all).
		next := treeActiveIdx
		switch {
		case treeActiveIdx == idx:
			next = idx - 1 // the tree's own tab is the one that died
		case treeActiveIdx > idx:
			next = treeActiveIdx - 1 // it shifted down one
		}
		// The id is authoritative whenever there IS one: it survives the reorder a
		// concurrent snapshot could apply between the capture and here, which the
		// ordinal arithmetic above cannot see.
		if treeActiveID != "" {
			if at, ok := inst.TabIndexByID(treeActiveID); ok {
				next = at
			}
		}
		if next < 0 {
			next = 0
		}
		m.store.SetActiveTab(next)
		m.clampSelectionTab()
		m.menu.SetActiveTab(m.store.ActiveTab())
		m.sidebar.SyncCursorToActiveTab()
	}
	return m, m.selectionChanged()
}

// handleTabJump jumps to a 1-based tab number (the 1-9 number keys). With a
// pane focused, the pane's own binding changes tab; with tree focus, the
// sidebar selection's active tab changes. Out-of-range numbers are a no-op
// (#930 PR 4). When the sidebar cursor rests on one of the selected instance's
// tab rows, it follows the tree-focus jump so the tree and active tab agree.
func (m *home) handleTabJump(oneBased int) (tea.Model, tea.Cmd) {
	idx := oneBased - 1
	if p := m.focusedOpenPane(); p != nil {
		w := m.paneWindows[p.ID()]
		if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
			m.cancelPanePreview(false)
		}
		if w == nil || !w.JumpToTab(idx) {
			return m, nil
		}
		// An explicit number-key jump is a COMMIT, not a peek: pin the pane's
		// intent for the current selection epoch so the trailing selectionChanged
		// (whose tree cursor still points at a different tab) and any background
		// tick cannot repaint the pane back onto that tab (#1885). Mirrors the web
		// #1862 layoutGeneration guard.
		//
		// Seed the epoch from the CURRENT selection first: on a restored/startup
		// pane the user can press a number before any selectionChanged has run, so
		// lastSelectionKey is still empty. Pinning against that unseeded epoch
		// pinned 0, and the jump's own trailing selectionChanged then read the
		// unchanged cursor as a move and bumped to 1 — staling the pin before the
		// guard ever saw it, and letting the repaint back in.
		m.bumpSelectionEpochIfMoved(m.sidebar.GetSelection())
		m.pinPaneJumpIntent(p.ID())
		// The pane's tab changed, so its live binding key changed — rebind it.
		m.syncLiveTermPane()
		return m, m.selectionChanged()
	}
	if idx < 0 || idx >= len(tree.TabLabels(m.store.GetSelectedInstance())) {
		return m, nil
	}
	m.store.SetActiveTab(idx)
	m.menu.SetActiveTab(idx)
	m.sidebar.SyncCursorToActiveTab()
	return m, m.selectionChanged()
}

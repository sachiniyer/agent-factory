package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Tab-roster accessors on Instance: stable-id lookup, ordinal resolution, and
// the shell-tab attach path. Extracted from instance.go to keep both files
// under the 1000-line lint limit (#1145).

// TabIDAt returns the stable id (#1738) of the tab at ordinal idx, and whether
// idx is in range. It is the index→id direction the data plane keys its per-tab
// broker on, so a broker follows its tab across a reorder/close instead of being
// pinned to a shifting ordinal.
func (i *Instance) TabIDAt(idx int) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if idx < 0 || idx >= len(i.Tabs) {
		return "", false
	}
	return i.Tabs[idx].ID, true
}

// tabTmuxTargetAt resolves an ordinal to both the stable broker key and its tmux
// target under one lock. Reading those in separate calls can pair one tab's ID
// with a sibling's tmux when a close shifts the roster between the calls (#2200).
// tabTmuxTargetAtLocked resolves an ordinal and its stable stream target in one
// snapshot. Callers must hold i.mu.
func (i *Instance) tabTmuxTargetAtLocked(idx int) (id string, ts *tmux.TmuxSession, exists bool) {
	if idx < 0 || idx >= len(i.Tabs) {
		return "", nil, false
	}
	tab := i.Tabs[idx]
	if !i.started {
		return tab.ID, nil, true
	}
	return tab.ID, tab.tmux, true
}

// tabTmuxByIDLocked resolves a tab's stable id (#1738) to the tmux session it
// currently backs, for callers coupling the resolved target to another
// lock-protected operation. Callers must hold i.mu.
func (i *Instance) tabTmuxByIDLocked(id string) (ts *tmux.TmuxSession, exists bool) {
	if id == "" {
		return nil, false
	}
	for idx, t := range i.Tabs {
		if t.ID != id {
			continue
		}
		if !i.started {
			return nil, true // the tab exists; it just has no live PTY yet
		}
		return i.tabTmuxAtLocked(idx), true
	}
	return nil, false
}

// TabTargetByID resolves a tab's stable id (#1738) DIRECTLY to what the web-tab
// proxy addresses it by — its kind, and the target URL a TabKindWeb tab stores —
// under a SINGLE lock acquisition. It is the tmux resolver's counterpart for the iframe
// plane, and exists for the same reason: id→ordinal followed by ordinal→tab takes
// i.mu TWICE, and a concurrent close between the two lands the second lookup on a
// DIFFERENT tab.
//
// A bounds check does not close that window, because the racing list is SHORTER,
// not out of range: with tabs [agent, A, B, C], resolving B yields ordinal 2, and
// a close of A before the second lookup leaves [agent, B, C] — where ordinal 2 is
// now C, in range and wrong. The proxy would then serve C's dev server under B's
// stable id, which is the exact misroute keying the route by id (#1810) exists to
// prevent.
//
// url is "" for every kind but TabKindWeb; a VSCODE tab deliberately stores none
// (its editor is resolved per request), so callers must not read absence of a URL
// as absence of a tab — that is what exists is for.
func (i *Instance) TabTargetByID(id string) (kind TabKind, url string, exists bool) {
	if id == "" {
		return 0, "", false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, t := range i.Tabs {
		if t.ID == id {
			return t.Kind, t.URL, true
		}
	}
	return 0, "", false
}

// TabIndexByID returns the CURRENT ordinal of the tab with stable id (#1738),
// and whether such a tab exists. It is the id→index resolution the stream
// endpoint runs per operation: a client addresses a tab by its stable id and the
// daemon maps it to wherever that tab now sits, so a reorder/close on another
// client can never make the client's captured position refer to a different tab.
// An empty id never matches (a legacy/absent id is not addressable by id).
//
// Prefer a single-lock primitive (TabTargetByID) where one exists
// for what the caller actually needs: an ordinal handed back to a SECOND lookup
// reopens the close/reorder window this resolution is meant to close.
func (i *Instance) TabIndexByID(id string) (int, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tabIndexByIDLocked(id)
}

// TabCount returns the number of tabs the instance currently holds.
func (i *Instance) TabCount() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.Tabs)
}

// TabTmuxName returns the sanitized tmux session name of the tab at idx, or
// "" when the instance is not started or the tab has no local session
// (remote tabs, out-of-range idx). The embedded terminal pane (#1089) uses it
// to attach its own render client to the tab's session; it never creates or
// mutates the session.
func (i *Instance) TabTmuxName(idx int) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if !i.started {
		return ""
	}
	ts := i.tabTmuxAtLocked(idx)
	if ts == nil {
		return ""
	}
	return ts.SanitizedName()
}

// AttachShellTab reconnects this local instance's in-memory tab list to a shell
// tab that already exists server-side — one the daemon's CreateTab RPC just
// spawned out-of-band (#960 PR 2). It is the no-spawn counterpart of
// AddShellTab: the daemon owns the spawn (so its authoritative view holds the
// tab and can't be clobbered), and the TUI only needs to reflect the new tab
// locally for instant display. It binds to the EXACT tmux session the daemon
// spawned and Restores (reconnects) it, mirroring restoreLocalTabs +
// LocalBackend.setupTabs, so the tab is immediately previewable/attachable
// without a second, colliding spawn.
//
// name and tmuxName are BOTH the daemon's, as returned by CreateTab. tmuxName is
// passed, not re-derived as "<agent>__<name>", because the two are independent
// namespaces (#1957, see tab_names.go): after a rename the daemon spawns
// "…__shell-2" for a tab named "shell", and re-deriving would bind this
// projection to the OLDER tab's still-live session. Empty falls back to the
// derivation — right for its only cause, a daemon predating the field.
//
// Local instances only — callers reject backends without TabManagement first. A
// tab with that name already present (a refresh raced ahead) makes this a no-op
// returning it. Errors when the instance is not started or has no session.
func (i *Instance) AttachShellTab(name, tmuxName, tabID string) (*Tab, error) {
	i.mu.Lock()
	if existing, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		i.mu.Unlock()
		return existing, err
	}
	started := i.started
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	title := i.Title
	i.mu.Unlock()
	if !started || agentTmux == nil || gw == nil {
		return nil, fmt.Errorf("cannot attach a tab to an instance that is not started")
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot attach a tab without a worktree")
	}

	// Bind to the exact session the daemon spawned and ATTACH-ONLY to it — never
	// spawn. Pass empty workDir so a session that is missing surfaces as an
	// error instead of re-spawning (#1152). The daemon is the single writer that
	// owns every tmux spawn (#960); this is a pure TUI-side projection of a tab
	// the daemon already created. If the daemon killed the instance in the window
	// since our RLock, the session is gone, and re-spawning it here would create a
	// tmux session that escapes the daemon's Kill teardown and orphans over the
	// about-to-be-deleted worktree — the same #990 leak AddShellTab guards. Fail
	// cleanly and let the daemon's next Snapshot reconcile the tab away.
	if tmuxName == "" {
		tmuxName = agentTmux.SanitizedName() + tmuxTabSeparator + name
	}
	shellTmux := agentTmux.NewSiblingSession(tmuxName, defaultShell())
	if err := shellTmux.Restore(""); err != nil {
		return nil, fmt.Errorf("failed to reconnect shell tab: %w", err)
	}

	tab := newShellTab(shellTmux)
	tab.Name = name
	// The daemon owns this tab's stable id (#1738). Empty is not a locally minted
	// substitute: it means an older daemon omitted the additive response field.
	tab.ID = tabID
	i.mu.Lock()
	if existing, ok, err := i.resolveAttachedTabLocked(name, tabID); ok || err != nil {
		i.mu.Unlock()
		if cerr := shellTmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("attach shell tab to %q: releasing raced attach client: %v", title, cerr)
		}
		return existing, err
	}
	// Re-check the teardown fence under the write lock before appending, mirroring
	// AddShellTab: Kill is not serialized against attach and can have flipped
	// started=false (snapshotting Tabs for teardown) in the window since our
	// RLock, and an archive teardown+move keeps started=true and raises OpArchiving
	// over that same window instead (#1195) — which the started-only recheck this
	// shipped with never saw, even though the caller gates on HasInFlightOp before
	// the daemon round-trip precisely because the op matters (#2100, the sibling of
	// the reconcile's missing recheck). Nothing was spawned above (attach-only), so
	// a lost race only needs to release the local attach client we opened and drop
	// the projection; the next reconcile re-adds the tab if it still exists
	// server-side.
	killed := !i.started || i.tabSpawnBlockedLocked() != nil
	if !killed {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	if killed {
		if cerr := shellTmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("attach shell tab to %q: releasing attach client after kill race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab attach")
	}
	return tab, nil
}

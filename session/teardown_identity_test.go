package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestClearClosedTmuxRefs_SurvivesACopyOnWriteReplace is #1987.
//
// teardownTabs snapshots the tabs it is about to close under i.mu, releases the
// lock to do the kills (they shell out to tmux and must not block readers), then
// re-locks to clear each closed tab's tmux ref. The ref clear was guarded by
// POINTER IDENTITY on the *Tab it captured.
//
// That guard is load-bearing for a real reason — it must not clobber a fresh
// session a concurrent Start swapped in — but it silently assumes the *Tab in
// i.Tabs is still the same OBJECT. Since #1904 it may not be: RenameTab and
// ReconcileTabsFromData write through replaceTabFieldLocked, which copies the
// Tab and stores a NEW pointer at i.Tabs[idx]. A rename landing inside
// teardown's unlock window therefore leaves the captured pointer pointing at a
// stale copy, and the clear nils the dead object while the LIVE tab keeps a ref
// to a session that has already been killed.
//
// This is exactly the failure the issue warns is quiet: no compile error, no
// panic, just a tab holding a dead session forever. It is not reachable in
// production today because the daemon's per-session op-lock serializes rename
// against kill/archive — but that lock lives in a different package, which
// session can neither see nor enforce, so the type must not depend on it.
//
// The fix is to key the ref clear on the tab's STABLE ID (#1738) instead of on
// pointer identity, keeping the tmux-session check as the concurrent-Start
// guard it always was.
func TestClearClosedTmuxRefs_SurvivesACopyOnWriteReplace(t *testing.T) {
	closing := &tmux.TmuxSession{}
	original := &Tab{ID: "tab-stable-id", Name: "shell", Kind: TabKindShell, tmux: closing}

	inst := &Instance{Title: "alpha", Tabs: []*Tab{original}}

	// What teardownTabs captures under the lock, before it releases it to kill.
	closed := []closedTab{{id: original.ID, name: original.Name, ts: closing}}

	// The unlock window: a rename arrives and writes through the copy-on-write
	// helper, so i.Tabs[0] is a DIFFERENT object carrying the same identity and
	// the same live tmux session.
	inst.replaceTabFieldLocked(0, func(c *Tab) { c.Name = "renamed" })
	live := inst.Tabs[0]
	if live == original {
		t.Fatal("premise broken: replaceTabFieldLocked did not replace the pointer, so this " +
			"test cannot reproduce #1987")
	}
	if live.tmux != closing {
		t.Fatal("premise broken: the copy did not carry the tmux session forward")
	}

	clearClosedTmuxRefs(inst, closed)

	// The live tab is the one whose session was killed, so it is the one that must
	// be cleared. Before #1987 this is the assertion that fails: the clear found
	// only the stale copy.
	if live.tmux != nil {
		t.Errorf("the LIVE tab still references the tmux session teardown already killed.\n\n"+
			"teardown captured *Tab %p and cleared that, but a copy-on-write rename had "+
			"replaced i.Tabs[0] with %p. Key the clear on the stable tab ID, not on pointer "+
			"identity (#1987).", original, live)
	}
}

// TestClearClosedTmuxRefs_LeavesAFreshSessionAlone pins the guard the #1987 fix
// must NOT lose. The whole reason the clear was identity-guarded is that a
// concurrent Start can swap a brand-new tmux session onto the tab while teardown
// is killing the old one; nil-ing that would strand a live session with no
// reference to it. Keying on the stable id makes the tab easier to FIND, which
// would make clobbering easier too if the session check were dropped with it.
func TestClearClosedTmuxRefs_LeavesAFreshSessionAlone(t *testing.T) {
	closing, restarted := &tmux.TmuxSession{}, &tmux.TmuxSession{}
	tab := &Tab{ID: "tab-stable-id", Name: "shell", Kind: TabKindShell, tmux: closing}
	inst := &Instance{Title: "alpha", Tabs: []*Tab{tab}}

	closed := []closedTab{{id: tab.ID, name: tab.Name, ts: closing}}

	// A concurrent Start wins the race and installs a fresh session on the SAME
	// tab (same id, same pointer) before finalize re-locks.
	tab.tmux = restarted

	clearClosedTmuxRefs(inst, closed)

	if tab.tmux != restarted {
		t.Errorf("cleared a tmux session teardown did not close: the tab now holds %v, want the "+
			"fresh session a concurrent Start installed. The id makes the tab findable; the "+
			"session check is what keeps the clear from clobbering.", tab.tmux)
	}
}

// TestClearClosedTmuxRefs_ClearsAClosedTabThatWasNotReplaced is the ordinary
// path — no rename, no restart — so the fix cannot pass the two cases above by
// simply never clearing anything.
func TestClearClosedTmuxRefs_ClearsAClosedTabThatWasNotReplaced(t *testing.T) {
	closing := &tmux.TmuxSession{}
	tab := &Tab{ID: "tab-stable-id", Name: "shell", Kind: TabKindShell, tmux: closing}
	inst := &Instance{Title: "alpha", Tabs: []*Tab{tab}}

	clearClosedTmuxRefs(inst, []closedTab{{id: tab.ID, name: tab.Name, ts: closing}})

	if tab.tmux != nil {
		t.Error("a closed tab that was never replaced must have its tmux ref cleared")
	}
}

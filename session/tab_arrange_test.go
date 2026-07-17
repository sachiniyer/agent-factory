package session

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rename/reorder unit tests (#1813). The headline case is
// TestRenameTab_FreesNoTmuxTokenForALaterSpawn: rename deliberately does NOT
// rename the live tmux session, so a tab's name and its tmux token decouple, and
// the collision that opens is the one bug this feature could introduce.

// tabNames returns the roster's display names, in order.
func tabNames(i *Instance) []string {
	names := make([]string, 0, len(i.Tabs))
	for _, t := range i.Tabs {
		names = append(names, t.Name)
	}
	return names
}

// TestRenameTab_FreesNoTmuxTokenForALaterSpawn is the regression test for the
// collision a rename opens up. A tab's tmux session name is derived at SPAWN
// ("<agent>__shell") and a rename does not touch it — restore rebinds by the
// persisted TmuxName, so the live session must keep its original name. That
// decouples the display name from the tmux token, and without reserving the
// token a later `t` would re-derive the SAME tmux name and collide with the
// renamed tab's still-live session:
//
//	Name="shell"/tmux "__shell" -> rename to "editor" -> `t` sees "shell" free
//	-> new tab derives "__shell" -> collision.
//
// The new tab must therefore get "shell-2"/"__shell-2".
func TestRenameTab_FreesNoTmuxTokenForALaterSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_collide_agent")

	// A shell tab named "shell", tmux session "af_collide_agent__shell".
	first, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, "shell", first.Name)
	firstTmux := first.tmux.SanitizedName()
	require.Equal(t, "af_collide_agent__shell", firstTmux)

	// Rename it. Shell tabs are not renameable through the daemon guard, so drive
	// the underlying rename the way a process tab would be renamed: the collision
	// is a property of the NAME/token decoupling, not of the kind.
	first.Kind = TabKindProcess
	resolved, err := inst.RenameTab(1, "editor")
	require.NoError(t, err)
	require.Equal(t, "editor", resolved)
	assert.Equal(t, firstTmux, first.tmux.SanitizedName(),
		"rename must leave the live tmux session's name alone — restore rebinds by it")

	// The `t` key: a second shell tab. "shell" LOOKS free (no tab is named it),
	// but the renamed tab's live session still owns the token.
	second, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.NotEqual(t, firstTmux, second.tmux.SanitizedName(),
		"new tab derived the renamed tab's live tmux session name — collision")
	assert.Equal(t, "shell-2", second.Name)
	assert.Equal(t, "af_collide_agent__shell-2", second.tmux.SanitizedName())
}

// TestRenameTab_TmuxTokenSurvivesSeparatorInName is the regression for the
// Codex finding on the token parse. sanitizeTabName keeps '_', so "logs__api" is
// a VALID tab name whose tmux session is "…__logs__api". The old parse split on
// the LAST "__" and reserved "api", leaving "logs__api" free — so a later
// tab-create --name logs__api derived the SAME session the renamed tab still
// owns and collided at spawn. The token must be the whole "logs__api".
func TestRenameTab_TmuxTokenSurvivesSeparatorInName(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_sep_agent")

	first, err := inst.AddProcessTab("btop", "logs__api")
	require.NoError(t, err)
	require.Equal(t, "logs__api", first.Name)
	firstTmux := first.tmux.SanitizedName()
	require.Equal(t, "af_sep_agent__logs__api", firstTmux)

	// Decouple name from token: rename away so "logs__api" is free as a NAME while
	// the live session is still bound to the "logs__api" token.
	_, err = inst.RenameTab(1, "editor")
	require.NoError(t, err)
	require.Equal(t, firstTmux, first.tmux.SanitizedName(), "rename leaves the live session alone")

	// Re-request the freed name. The reserved token is the whole "logs__api", so
	// the new tab must be suffixed rather than deriving the live session's name.
	second, err := inst.AddProcessTab("btop", "logs__api")
	require.NoError(t, err)
	assert.NotEqual(t, firstTmux, second.tmux.SanitizedName(),
		"new tab derived the renamed tab's live session name — the separator-in-name collision")
	assert.Equal(t, "logs__api-2", second.Name)
	assert.Equal(t, "af_sep_agent__logs__api-2", second.tmux.SanitizedName())
}

// TestRenameTab_ReclaimsItsOwnToken: excluding the renamed tab covers its token
// as well as its name, so a tab can be renamed back to what it was. Nothing else
// can have taken the name in the meantime — it was reserved the whole time the
// tab held it.
func TestRenameTab_ReclaimsItsOwnToken(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_reclaim_agent")
	tab, err := inst.AddProcessTab("btop", "logs")
	require.NoError(t, err)
	require.Equal(t, "logs", tab.Name)

	renamed, err := inst.RenameTab(1, "metrics")
	require.NoError(t, err)
	require.Equal(t, "metrics", renamed)

	back, err := inst.RenameTab(1, "logs")
	require.NoError(t, err)
	assert.Equal(t, "logs", back, "a tab must be able to reclaim its own former name unsuffixed")
}

// TestRenameTab_DoesNotMutateHandedOutTabs is the race regression for the
// GetTabs contract. GetTabs copies the SLICE and hands out the live *Tab
// pointers, and its contract is that the elements' Name is safe to read WITHOUT
// holding i.mu — which is exactly what the render path (tree.TabLabels) and the
// TUI's tabNameAt/tabIndexByName do. A rename that assigned tab.Name in place
// wrote the very field those readers read off-lock.
//
// Run under -race, the reader goroutine below is the documented usage and the
// writer is the daemon's rename; copy-on-write is what makes the pair legal.
// Without it the detector fires on Tab.Name.
func TestRenameTab_DoesNotMutateHandedOutTabs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_racy_agent")
	_, err := inst.AddProcessTab("btop", "logs")
	require.NoError(t, err)

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < rounds; n++ {
			// The off-lock read GetTabs promises is safe — tree.TabLabels' loop.
			for _, tab := range inst.GetTabs() {
				_ = tab.Name
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < rounds; n++ {
			if _, rerr := inst.RenameTab(1, fmt.Sprintf("logs-%d", n)); rerr != nil {
				assert.NoError(t, rerr)
				return
			}
		}
	}()
	wg.Wait()
}

// TestRenameTab_SnapshotKeepsItsValue is the observable half of the same defect,
// and holds without -race. A caller that snapshots the roster and reads a name
// later must see the value that was there when it looked — an in-place rename
// teleported the new name into a snapshot taken before the rename happened.
func TestRenameTab_SnapshotKeepsItsValue(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_snap_agent")
	_, err := inst.AddProcessTab("btop", "logs")
	require.NoError(t, err)

	before := inst.GetTabs()
	require.Equal(t, "logs", before[1].Name)

	_, err = inst.RenameTab(1, "metrics")
	require.NoError(t, err)

	assert.Equal(t, "logs", before[1].Name,
		"a snapshot taken before the rename must still read the old name — GetTabs hands out tabs whose Name is never mutated")
	assert.Equal(t, "metrics", inst.GetTabs()[1].Name, "the next snapshot carries the new name")
}

// TestReorderTab_DoesNotMutateHandedOutTabs is the sibling audit. ReorderTab
// assigns a fresh SLICE rather than mutating any *Tab, so a concurrent GetTabs
// copies either the old order or the new one under RLock and never observes a
// torn roster. This pins that: a reorder must stay a slice-level operation, so
// nobody later "optimizes" it into an in-place field shuffle.
func TestReorderTab_DoesNotMutateHandedOutTabs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_racy_reorder_agent")
	_, err := inst.AddProcessTab("btop", "a")
	require.NoError(t, err)
	_, err = inst.AddProcessTab("htop", "b")
	require.NoError(t, err)

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < rounds; n++ {
			for _, tab := range inst.GetTabs() {
				_ = tab.Name
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < rounds; n++ {
			from, to := 1, 2
			if n%2 == 1 {
				from, to = 2, 1
			}
			if rerr := inst.ReorderTab(from, to); rerr != nil {
				assert.NoError(t, rerr)
				return
			}
		}
	}()
	wg.Wait()
}

// TestRenameTab_SuffixesOnCollision: rename resolves names through the same
// uniqueness rule as create, so renaming onto a taken name suffixes rather than
// producing two identically-named tabs (which every other verb addresses by
// name).
func TestRenameTab_SuffixesOnCollision(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_dup_agent")
	_, err := inst.AddProcessTab("btop", "dup")
	require.NoError(t, err)
	other, err := inst.AddProcessTab("htop", "other")
	require.NoError(t, err)
	require.Equal(t, "other", other.Name)

	resolved, err := inst.RenameTab(2, "dup")
	require.NoError(t, err)
	assert.Equal(t, "dup-2", resolved)
	assert.Equal(t, []string{"agent", "dup", "dup-2"}, tabNames(inst))
}

// TestRenameTab_Sanitizes: a requested name goes through the same tmux-safe
// sanitization as tab-create's --name.
func TestRenameTab_Sanitizes(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_sanitize_agent")
	_, err := inst.AddProcessTab("btop", "proc")
	require.NoError(t, err)

	resolved, err := inst.RenameTab(1, "my tab:2")
	require.NoError(t, err)
	assert.Equal(t, "my-tab-2", resolved)
}

// TestRenameTab_RejectsSanitizeToEmpty: a name with nothing usable in it is an
// error, NOT a silent fall back to a default. #1813 calls the silent mangling
// out as a wart; a user who typed "...." asked for something specific, and
// quietly naming the tab "web" instead would be the same wart moved.
func TestRenameTab_RejectsSanitizeToEmpty(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_empty_agent")
	tab, err := inst.AddProcessTab("btop", "proc")
	require.NoError(t, err)

	_, err = inst.RenameTab(1, "....")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no usable characters")
	assert.Equal(t, "proc", tab.Name, "a rejected rename must leave the name untouched")
}

// TestRenameTab_RejectsUndisplayedKinds: agent and shell tabs render fixed
// labels ("Agent"/"Terminal") on every surface, so a rename would write a field
// nothing reads. Refuse rather than report a success the user cannot see.
func TestRenameTab_RejectsUndisplayedKinds(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_kinds_agent")
	shell, err := inst.AddShellTab()
	require.NoError(t, err)

	_, err = inst.RenameTab(0, "boss")
	assert.Error(t, err, "the agent tab must not be renameable")

	_, err = inst.RenameTab(1, "editor")
	assert.Error(t, err, "a shell tab must not be renameable")
	assert.Equal(t, "shell", shell.Name)
}

// TestTabKindRenameable states the predicate's expected answer for every kind:
// exactly the kinds whose label displays Name.
//
// It RESTATES the rule; it cannot CHECK it. The mapping this predicate mirrors
// lives in ui/tree/labels.go textForTab, and tree imports session, so session
// can never reach textForTab to compare against — the import cycle forbids it.
// The mechanical check lives on the other side of that boundary, in
// ui/tree.TestRenameableTracksLabel, which drives textForTab directly. Keep both:
// this one fails fast beside the predicate, that one is the one that can't lie.
func TestTabKindRenameable(t *testing.T) {
	assert.False(t, TabKindRenameable(TabKindAgent), "agent tabs always render \"Agent\"")
	assert.False(t, TabKindRenameable(TabKindShell), "shell tabs always render \"Terminal\"")
	assert.True(t, TabKindRenameable(TabKindProcess))
	assert.True(t, TabKindRenameable(TabKindWeb))
	assert.True(t, TabKindRenameable(TabKindVSCode), "vscode tabs render Name || \"VS Code\" (#1817)")
}

// TestReorderTab_Permutes drives the move semantics: the destination index is
// read in the FINAL roster, so moving 1 -> 3 of four tabs puts the tab last.
func TestReorderTab_Permutes(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_move_agent")
	for _, name := range []string{"a", "b", "c"} {
		_, err := inst.AddProcessTab("btop", name)
		require.NoError(t, err)
	}
	require.Equal(t, []string{"agent", "a", "b", "c"}, tabNames(inst))

	require.NoError(t, inst.ReorderTab(1, 3))
	assert.Equal(t, []string{"agent", "b", "c", "a"}, tabNames(inst))

	// And back: moving it from its new index to its old one is the exact inverse
	// — the property the daemon's persist-failure rollback relies on.
	require.NoError(t, inst.ReorderTab(3, 1))
	assert.Equal(t, []string{"agent", "a", "b", "c"}, tabNames(inst))
}

// TestReorderTab_PinsAgentSlot is the invariant test. Tabs[0] IS the agent tab
// to the rest of the package — archive teardown keeps Tabs[0], the agent
// conversation and the agent tmux session are read off it — so permuting slot 0
// in either direction would silently re-point all of that at a shell tab. This
// is correctness, not display preference.
func TestReorderTab_PinsAgentSlot(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_pin_agent")
	for _, name := range []string{"a", "b"} {
		_, err := inst.AddProcessTab("btop", name)
		require.NoError(t, err)
	}

	assert.Error(t, inst.ReorderTab(0, 2), "the agent tab must not be movable")
	assert.Error(t, inst.ReorderTab(2, 0), "no tab may be moved in front of the agent tab")
	assert.Error(t, inst.ReorderTab(1, 3), "out-of-range destination must be refused")
	assert.Error(t, inst.ReorderTab(3, 1), "out-of-range source must be refused")
	assert.Equal(t, []string{"agent", "a", "b"}, tabNames(inst), "every rejected move must leave the order intact")
	assert.Equal(t, TabKindAgent, inst.Tabs[0].Kind)
}

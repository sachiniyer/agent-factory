package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1957: a renamed tab kept its OLD name reserved, so `tab-create --name fresh`
// answered "fresh-2" with nothing on the roster named "fresh". The reservation
// was real — the renamed tab's tmux session still holds "…__fresh" — but it was
// charged to the wrong namespace. These tests pin the split: a NAME is taken
// only while a tab actually holds it, and the still-live tmux session is dodged
// at SPAWN instead. See the note at the top of tab_names.go.

// TestRenameTab_FreesTheOldNameForANewTab is the #1957 repro, verbatim from the
// issue: rename the tab named "fresh" to "fresh-old", then ask for "fresh". The
// answer must be "fresh" — nothing on the roster is named it — and the new tab's
// tmux session must NOT be the one the renamed tab is still running in.
func TestRenameTab_FreesTheOldNameForANewTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_agent")

	first, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	require.Equal(t, "fresh", first.Name)
	firstTmux := first.tmux.SanitizedName()
	require.Equal(t, "af_1957_agent__fresh", firstTmux)

	renamed, err := inst.RenameTab(1, "fresh-old")
	require.NoError(t, err)
	require.Equal(t, "fresh-old", renamed)
	require.Equal(t, firstTmux, first.tmux.SanitizedName(),
		"rename must leave the live tmux session alone — restore rebinds by it")

	second, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh", second.Name,
		"the name the renamed tab gave up must be handed straight back, unsuffixed")
	assert.Equal(t, "af_1957_agent__fresh-2", second.tmux.SanitizedName(),
		"the SPAWN takes the suffix instead, so it misses the renamed tab's live session")
	assert.NotEqual(t, firstTmux, second.tmux.SanitizedName(),
		"new tab derived the renamed tab's live tmux session name — collision")
	assert.Equal(t, []string{"agent", "fresh-old", "fresh"}, tabNames(inst))
}

// TestAddTab_SuffixesANameALiveTabStillHolds is the no-over-freeing half. The
// fix frees a name its tab gave up; it must not free one a tab is still using.
func TestAddTab_SuffixesANameALiveTabStillHolds(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_live")

	first, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	require.Equal(t, "fresh", first.Name)

	// No rename this time: "fresh" is a name a live tab answers to.
	second, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh-2", second.Name,
		"a name a live tab still holds must still collide")
	assert.Equal(t, "af_1957_live__fresh-2", second.tmux.SanitizedName())

	// And a third stacks, rather than reusing either taken name.
	third, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh-3", third.Name)
	assert.Equal(t, "af_1957_live__fresh-3", third.tmux.SanitizedName())
}

// TestRenameTab_CreateRenameRoundTrips walks the full cycle the issue's user
// would: rename away, recreate under the freed name, rename the NEW tab, and
// recreate again. Every step must hand back the exact name asked for, and every
// tab must end up in a tmux session of its own.
func TestRenameTab_CreateRenameRoundTrips(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_round")

	first, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	require.Equal(t, "af_1957_round__fresh", first.tmux.SanitizedName())

	name, err := inst.RenameTab(1, "fresh-old")
	require.NoError(t, err)
	require.Equal(t, "fresh-old", name)

	second, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	require.Equal(t, "fresh", second.Name)
	require.Equal(t, "af_1957_round__fresh-2", second.tmux.SanitizedName())

	// Rename the NEW tab away too; "fresh" is free again for a third.
	name, err = inst.RenameTab(2, "fresh-older")
	require.NoError(t, err)
	require.Equal(t, "fresh-older", name)

	third, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh", third.Name)
	assert.Equal(t, "af_1957_round__fresh-3", third.tmux.SanitizedName(),
		"two live sessions now hold the earlier tokens, so the spawn walks past both")

	assert.Equal(t, []string{"agent", "fresh-old", "fresh-older", "fresh"}, tabNames(inst))
	assert.Len(t, distinctTmuxNames(inst), 4, "every tab must own a distinct tmux session")
}

// TestRenameTab_FreedNameIsFreeForATmuxlessTab: a web/vscode tab owns no tmux
// session, so nothing was ever protecting it from the freed name — it was purely
// collateral damage from the shared namespace. It must take the name too.
func TestRenameTab_FreedNameIsFreeForATmuxlessTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_web")

	_, err := inst.AddProcessTab("btop", "dashboard")
	require.NoError(t, err)
	name, err := inst.RenameTab(1, "dashboard-old")
	require.NoError(t, err)
	require.Equal(t, "dashboard-old", name)

	web, err := inst.AddWebTab("http://localhost:3000", "dashboard")
	require.NoError(t, err)
	assert.Equal(t, "dashboard", web.Name)

	vscode, err := inst.AddVSCodeTab("editor")
	require.NoError(t, err)
	require.Equal(t, "editor", vscode.Name)
	renamedVSCode, err := inst.RenameTab(3, "editor-old")
	require.NoError(t, err)
	require.Equal(t, "editor-old", renamedVSCode)

	again, err := inst.AddVSCodeTab("editor")
	require.NoError(t, err)
	assert.Equal(t, "editor", again.Name,
		"a tmux-less tab's freed name has nothing live behind it at all")
}

// TestCloseTab_FreesNameAndTmuxToken is the adjacent-verb check. Close kills the
// tab's tmux session, so BOTH namespaces free up and the next tab gets the plain
// name and the plain session — no lingering "-2" from a tab that is gone.
func TestCloseTab_FreesNameAndTmuxToken(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_close")

	first, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	require.Equal(t, "af_1957_close__fresh", first.tmux.SanitizedName())
	require.NoError(t, inst.CloseTab(1))

	second, err := inst.AddProcessTab("btop", "fresh")
	require.NoError(t, err)
	assert.Equal(t, "fresh", second.Name)
	assert.Equal(t, "af_1957_close__fresh", second.tmux.SanitizedName(),
		"the closed tab's session is dead, so its token is free to retake")
}

// TestReorderTab_TouchesNeitherNamespace is the other adjacent verb: a reorder
// only permutes the slice, so no name and no tmux token changes hands and a
// later create resolves exactly as it would have before.
func TestReorderTab_TouchesNeitherNamespace(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_1957_reorder")

	for _, n := range []string{"alpha", "beta"} {
		_, err := inst.AddProcessTab("btop", n)
		require.NoError(t, err)
	}
	require.NoError(t, inst.ReorderTab(2, 1))
	require.Equal(t, []string{"agent", "beta", "alpha"}, tabNames(inst))

	next, err := inst.AddProcessTab("btop", "alpha")
	require.NoError(t, err)
	assert.Equal(t, "alpha-2", next.Name, "both names are still held by live tabs")
	assert.Equal(t, "af_1957_reorder__alpha-2", next.tmux.SanitizedName())
}

// distinctTmuxNames returns the set of tmux session names the roster's tabs hold.
func distinctTmuxNames(i *Instance) map[string]bool {
	names := map[string]bool{}
	for _, t := range i.Tabs {
		if t.tmux != nil {
			names[t.tmux.SanitizedName()] = true
		}
	}
	return names
}

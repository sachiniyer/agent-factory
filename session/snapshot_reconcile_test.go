package session

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReconcileTestInstance builds a started local instance with a single mock-
// backed agent tab and a worktree, hermetically (no real tmux server). The
// shared persistPtyFactory/nameKeyedExec helpers (tab_persist_test.go) make the
// agent session — and any sibling the reconcile reconnects — report alive so
// Restore reconnects rather than spawning.
func newReconcileTestInstance(t *testing.T, agentName string, alive map[string]bool) (*Instance, string) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)
	// Isolate config reads from the developer's real ~/.agent-factory (see
	// tab_persist_test.go for the full #837 rationale).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	exec := nameKeyedExec(alive)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	worktreePath := filepath.Join(t.TempDir(), "wt")
	gw, err := git.NewGitWorktreeFromStorage(
		"/tmp/snap-reconcile-repo", worktreePath, "snap",
		"snap-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	inst := &Instance{
		Title:       "snap",
		Path:        "/tmp/snap-reconcile-repo",
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}
	return inst, worktreePath
}

// TestReconcileTabsFromData_AddsOutOfBandTab is the #959 live-display reconnect:
// the daemon spawned a shell tab out-of-band, so the snapshot's tab list grows;
// ReconcileTabsFromData must add the tab to the live instance and reconnect it to
// its EXACT persisted tmux session by name, leaving it immediately attachable.
func TestReconcileTabsFromData_AddsOutOfBandTab(t *testing.T) {
	const agentName = "af_snap_agent"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	require.Len(t, inst.GetTabs(), 1, "instance starts with only the agent tab")

	target := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}

	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.True(t, changed, "adding an out-of-band tab is a change")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "the out-of-band shell tab must appear on the live instance")
	assert.Equal(t, TabKindShell, tabs[1].Kind)
	assert.Equal(t, shellName, tabs[1].tmux.SanitizedName(),
		"the added tab must bind to its EXACT persisted tmux session (reconnect by name)")
	assert.True(t, inst.TabAlive(1), "the reconnected tab must be live (attachable) without a restart")

	// Reconciling the same target again is a no-op (no flicker / repaint).
	changedAgain, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.False(t, changedAgain, "an unchanged snapshot must not report a change")
	assert.Len(t, inst.GetTabs(), 2, "no duplicate tab on a repeat reconcile")
}

// TestReconcileTabsFromData_DropsClosedTab covers the close side: the daemon
// closed a tab out-of-band (it leaves the snapshot), so the live instance must
// drop it locally WITHOUT re-killing the already-gone tmux session.
func TestReconcileTabsFromData_DropsClosedTab(t *testing.T) {
	const agentName = "af_snap_agent2"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	// Add the shell tab first.
	withShell := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}
	_, err := inst.ReconcileTabsFromData(withShell)
	require.NoError(t, err)
	require.Len(t, inst.GetTabs(), 2)

	// The daemon closed it: the snapshot now lists only the agent tab.
	agentOnly := []TabData{{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName}}
	changed, err := inst.ReconcileTabsFromData(agentOnly)
	require.NoError(t, err)
	assert.True(t, changed, "dropping a closed tab is a change")
	require.Len(t, inst.GetTabs(), 1, "the closed tab must be dropped locally")
	assert.Equal(t, TabKindAgent, inst.GetTabs()[0].Kind, "the agent tab is never dropped")
}

// TestReconcileTabsFromData_AddsTmuxlessWebTab is the post-merge Codex finding on
// #1815: a web tab holds NO tmux session by design, and this loop used to skip
// every target tab with an empty TmuxName — reading "" as "this tab's session is
// missing" rather than "this kind never had one". So the headline #1815 case (an
// agent injecting a live browser view with `tab-create --kind web`) published a
// roster the TUI then silently discarded: the tab stayed invisible until a full
// rebuild, even though the DROP side already removed such a tab by name.
func TestReconcileTabsFromData_AddsTmuxlessWebTab(t *testing.T) {
	const agentName = "af_snap_web"
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true})

	const url = "http://localhost:5173"
	target := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "livepreview", Kind: TabKindWeb, URL: url},
	}

	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.True(t, changed, "an out-of-band web tab is a change")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "a web tab created out-of-band must appear on the live instance")
	assert.Equal(t, TabKindWeb, tabs[1].Kind)
	assert.Equal(t, url, tabs[1].URL, "the web tab's URL must survive the reconcile, or the pane has nothing to show")
	assert.Nil(t, tabs[1].tmux, "a web tab holds no tmux session")
	assert.NotEmpty(t, tabs[1].ID, "the reconciled tab must be addressable by a stable id (#1738)")

	changedAgain, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.False(t, changedAgain, "an unchanged snapshot must not report a change")
	assert.Len(t, inst.GetTabs(), 2, "no duplicate web tab on a repeat reconcile")
}

// TestReconcileTabsFromData_AddsTmuxlessVSCodeTab: the same gap covered the vscode
// kind (#1817), which is tmux-less for the same reason and was skipped by the same
// branch. It carries no URL by design — its code-server target is resolved at proxy
// time — so an empty URL here is correct, not a dropped field.
func TestReconcileTabsFromData_AddsTmuxlessVSCodeTab(t *testing.T) {
	const agentName = "af_snap_code"
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true})

	target := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "vscode", Kind: TabKindVSCode},
	}

	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	assert.True(t, changed, "an out-of-band vscode tab is a change")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "a vscode tab created out-of-band must appear on the live instance")
	assert.Equal(t, TabKindVSCode, tabs[1].Kind)
	assert.Nil(t, tabs[1].tmux, "a vscode tab holds no tmux session")
}

// TestReconcileTabsFromData_AdoptsTmuxlessTabID pins that a tmux-less tab keeps the
// daemon's authoritative stable id (#1738) rather than minting a local one. The
// sibling tests above assert the id is merely non-empty, which a locally-minted id
// also satisfies — but the id is what a web-UI stream/pane binding addresses the tab
// by, so a local mint would resolve to nothing on the daemon. A PTY-backed tab gets
// this for free by reconnecting to a named session; a tmux-less one has no such
// anchor, and the append is the only place its identity can come from.
func TestReconcileTabsFromData_AdoptsTmuxlessTabID(t *testing.T) {
	const agentName = "af_snap_meta_id"
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true})

	target := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "daemon-id-abc", Name: "editor", Kind: TabKindVSCode},
	}
	_, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2)
	assert.Equal(t, "daemon-id-abc", tabs[1].ID, "the daemon owns tab identity; the reconcile must adopt its id")
}

// TestReconcileTabsFromData_SkipsTmuxfulTabWithNoSession pins the other half of
// TabKind.HasTmux: the empty-TmuxName skip must SURVIVE for a kind that is
// supposed to own a PTY. Such a record is not reconstructable, and materializing
// it would put a terminal tab with no process behind it in the TUI.
func TestReconcileTabsFromData_SkipsTmuxfulTabWithNoSession(t *testing.T) {
	const agentName = "af_snap_noname"
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true})

	changed, err := inst.ReconcileTabsFromData([]TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{Name: "shell", Kind: TabKindShell}, // a PTY kind with no session to bind
	})
	require.NoError(t, err)
	assert.False(t, changed, "a tmux-ful tab with no session must be skipped, not materialized")
	assert.Len(t, inst.GetTabs(), 1)
}

// TestReconcileTabsFromData_NotStartedIsNoOp guards the not-started branch: an
// unstarted instance (e.g. a Loading placeholder) must never attempt a reconnect.
func TestReconcileTabsFromData_NotStartedIsNoOp(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "pending", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	changed, rerr := inst.ReconcileTabsFromData([]TabData{
		{Name: "agent", Kind: TabKindAgent},
		{Name: "shell", Kind: TabKindShell, TmuxName: "whatever__shell"},
	})
	require.NoError(t, rerr)
	assert.False(t, changed, "a not-started instance reconcile is a no-op")
}

// TestReconcileTabsFromData_RenamesInPlaceByID is the #1905 fix at the roster
// layer: when the snapshot renames a tab (same stable id, new display name), the
// reconcile must relabel the SAME tab object in place — keeping its slot and its
// live tmux session — rather than reading the changed name as "old tab gone, new
// tab added" and dropping+re-adding it at the end of the roster (which would blip
// the PTY, reorder the tab, and close any pane bound to it).
func TestReconcileTabsFromData_RenamesInPlaceByID(t *testing.T) {
	const agentName = "af_snap_rename"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	// Add a shell tab carrying a stable id (the daemon owns the id).
	add := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "sid-1", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	}
	changed, err := inst.ReconcileTabsFromData(add)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, inst.GetTabs(), 2)
	shellTab := inst.GetTabs()[1]
	shellTmux := shellTab.tmux
	require.Equal(t, "sid-1", shellTab.ID)
	require.NotNil(t, shellTmux, "precondition: the shell tab has a live tmux session")

	// Rename: same id, new name.
	renamed := []TabData{
		{Name: inst.GetTabs()[0].Name, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "sid-1", Name: "editor", Kind: TabKindShell, TmuxName: shellName},
	}
	changed, err = inst.ReconcileTabsFromData(renamed)
	require.NoError(t, err)
	assert.True(t, changed, "a rename is a change (the label moved)")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "a rename must NOT add or drop a tab — the roster length is unchanged")
	assert.Equal(t, "editor", tabs[1].Name, "the tab now shows the new name")
	assert.Equal(t, "sid-1", tabs[1].ID, "the stable id is unchanged")
	assert.Same(t, shellTmux, tabs[1].tmux, "the live tmux session must be preserved (no PTY blip)")
	// The relabel is copy-on-write, so the slot holds a NEW *Tab (readers hold the
	// old pointer unlocked — mutating Name in place would race them). The
	// identity that matters is the id + the live session above, both carried over.
	assert.NotSame(t, shellTab, tabs[1], "the relabel must copy-on-write, never mutate the live tab in place")
	assert.Equal(t, "shell", shellTab.Name, "the pointer a reader already held keeps its consistent old value")

	// Reconciling the renamed roster again is a no-op.
	changedAgain, err := inst.ReconcileTabsFromData(renamed)
	require.NoError(t, err)
	assert.False(t, changedAgain, "a settled rename must not report a change on the next poll")
}

// TestReconcileTabsFromData_CloseRecreateSameNameIsIDKeyed is the #1886 fix at
// the roster layer (codex P1 on #1906): another client closed a tab and created a
// new one that REUSED the freed display name, so the roster's row has the same
// name but a NEW stable id. A name-keyed reconcile saw "unchanged", reported
// changed=false, and silently re-pointed the local tab's id at the new tab — so
// the TUI's pane reconcile never ran and the pane stayed open on a tab that no
// longer exists, showing a different process. Keyed on the id this is a drop of
// the old id plus an add of the new one, and it MUST report a change so the pane
// layer can close the orphaned pane.
func TestReconcileTabsFromData_CloseRecreateSameNameIsIDKeyed(t *testing.T) {
	const agentName = "af_snap_recreate"
	shellName := agentName + shellTmuxSuffix
	inst, _ := newReconcileTestInstance(t, agentName, map[string]bool{agentName: true, shellName: true})

	agent := inst.GetTabs()[0].Name
	changed, err := inst.ReconcileTabsFromData([]TabData{
		{Name: agent, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "id-old", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, inst.GetTabs(), 2)
	require.Equal(t, "id-old", inst.GetTabs()[1].ID)

	// The out-of-band close+recreate: same name, NEW id.
	changed, err = inst.ReconcileTabsFromData([]TabData{
		{Name: agent, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "id-new", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	})
	require.NoError(t, err)
	assert.True(t, changed,
		"a same-name/new-id recreate MUST report a change — the TUI gates its pane reconcile on it (#1886)")

	tabs := inst.GetTabs()
	require.Len(t, tabs, 2, "the recreated tab replaces the old one; the roster length is unchanged")
	assert.Equal(t, "id-new", tabs[1].ID,
		"the surviving tab is the NEW one; the old id must be gone, never re-pointed onto the new tab")
	assert.Equal(t, "shell", tabs[1].Name)

	// Settled: the same roster again is a no-op (no drop/add churn per poll).
	changedAgain, err := inst.ReconcileTabsFromData([]TabData{
		{Name: agent, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "id-new", Name: "shell", Kind: TabKindShell, TmuxName: shellName},
	})
	require.NoError(t, err)
	assert.False(t, changedAgain, "a settled roster must not churn on the next poll")
}

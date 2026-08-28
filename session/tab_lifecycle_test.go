package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startedMockInstance builds a started local instance whose agent tab is a
// mock-backed tmux session (#930 PR 4). AddShellTab spawns siblings off that
// session, so they inherit the mock PTY factory / executor and stay hermetic —
// no real tmux server is touched. extraAlive marks additional session names as
// already existing (used by the restart-survival test).
func startedMockInstance(t *testing.T, agentName string, extraAlive ...string) *Instance {
	t.Helper()
	// Isolate config reads from the developer's real ~/.agent-factory (#837).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	alive := map[string]bool{agentName: true}
	for _, n := range extraAlive {
		alive[n] = true
	}
	exec := nameKeyedExec(alive)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	repoPath := "/tmp/tab-lifecycle-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	return &Instance{
		Title:       agentName,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}
}

// TestFreshStart_OnlyAgentTab is the #1100 headline test, through the
// production path (NewInstance -> Start(true) -> setupTabs): creating a new
// instance must bring up ONLY the agent tab — no terminal (shell) tab is
// auto-created and no __shell tmux session is spawned. The terminal tab stays
// available on demand via AddShellTab (the 't' hotkey / `af sessions
// tab-create` path).
func TestFreshStart_OnlyAgentTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	workdir := t.TempDir()
	runGit(t, workdir, "init")
	runGit(t, workdir, "config", "--local", "user.email", "test@example.com")
	runGit(t, workdir, "config", "--local", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "f.txt"), []byte("x"), 0644))
	runGit(t, workdir, "add", ".")
	runGit(t, workdir, "commit", "-m", "init")

	const agentName = "af_1100_fresh"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}

	inst, err := NewInstance(InstanceOptions{Title: "fresh-1100", Path: workdir, Program: "bash"})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	tabs := inst.GetTabs()
	require.Len(t, tabs, 1, "a fresh instance must come up with only the agent tab (#1100)")
	assert.Equal(t, TabKindAgent, tabs[0].Kind)

	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		agentName+tmuxTabSeparator+shellTabName, defaultShell(), pty, cmdExec)
	assert.False(t, shellTs.ExistsOrUnknown(),
		"no __shell tmux session may be spawned on a fresh start (#1100)")

	// The terminal tab is still available on demand.
	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the on-demand shell tab must be live")
}

// TestAddShellTab_AppendsAndNamesUniquely verifies a human-created shell tab is
// appended, named uniquely per instance ("shell", then "shell-2"), and backed by
// a distinct live tmux session.
func TestAddShellTab_AppendsAndNamesUniquely(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_add")

	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, shellTabName, tab.Name, "first shell tab is named %q", shellTabName)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the new shell tab session must be live")

	tab2, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, "shell-2", tab2.Name, "the second shell tab must get a unique name")
	assert.Equal(t, 3, inst.TabCount())
	assert.True(t, inst.TabAlive(2))
	assert.NotEqual(t, tab.tmux.SanitizedName(), tab2.tmux.SanitizedName(),
		"each shell tab must have a unique tmux session name")
}

func TestAddShellTab_UsesAccountScopedShellLaunch(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	inst := startedMockInstance(t, "af_tabs_account_shell")
	inst.Account = "work"
	inst.Tabs[0].tmux.SetAccountForAgent("claude", "work")

	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, "/bin/sh -i", tab.tmux.Program(),
		"an account-scoped terminal must not source startup files that can replace its selected identity")
}

// TestCloseTab_RemovesAndProtectsAgent verifies CloseTab removes a shell tab and
// kills its session, but refuses to close the agent tab (index 0) or any
// out-of-range index.
func TestCloseTab_RemovesAndProtectsAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_close")
	_, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount())

	require.Error(t, inst.CloseTab(0), "the agent tab must be unclosable")
	require.Equal(t, 2, inst.TabCount(), "a rejected close must not mutate the tab list")

	require.Error(t, inst.CloseTab(9), "an out-of-range index must be rejected")
	require.Equal(t, 2, inst.TabCount())

	require.NoError(t, inst.CloseTab(1))
	require.Equal(t, 1, inst.TabCount(), "closing a shell tab removes it")
	require.False(t, inst.TabAlive(1), "the closed tab's session must be gone")
}

// TestAddShellTab_HasNoTabCountCap is the inverse of the guard it replaces, and
// the count is chosen to matter: 12 is past the old 9-tab cap AND past the 1-9
// number-key range that justified it. #930 PR 4 let the KEYBOARD limit the DATA —
// a session could not hold a tenth tab because there was no tenth key to jump to
// it with. Reaching a tab is navigation (#3021); refusing to create it is a worse
// answer, and it is the one users hit (#3023).
func TestAddShellTab_HasNoTabCountCap(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_cap")
	// Agent tab + 11 shell tabs = 12, three past the old cap.
	for i := 0; i < 11; i++ {
		_, err := inst.AddShellTab()
		require.NoErrorf(t, err, "tab %d must be created; there is no cap", i+2)
	}
	require.Equal(t, 12, inst.TabCount())

	// Names stay unique past the old ceiling — the uniquifier used to be exercised
	// only up to "shell-9", so two-digit suffixes are genuinely new ground.
	names := map[string]bool{}
	for _, tab := range inst.GetTabs() {
		require.False(t, names[tab.Name], "duplicate tab name %q past the old cap", tab.Name)
		names[tab.Name] = true
	}
}

// TestAddShellTab_RejectedForUnstarted verifies AddShellTab errors when the
// instance has no live agent session/worktree (e.g. not started).
func TestAddShellTab_RejectedForUnstarted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := NewInstance(InstanceOptions{Title: "unstarted", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	_, err = inst.AddShellTab()
	require.Error(t, err)
	require.Equal(t, 0, inst.TabCount())
}

// TestAttachShellTab_ReconnectsExistingSessionNoSpawn verifies the no-spawn
// reconnect path (#960 PR 2): when the daemon has already created the shell
// session, AttachShellTab binds to that exact session and appends the tab
// without issuing a second new-session that would collide.
func TestAttachShellTab_ReconnectsExistingSessionNoSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_tabs_attach"
	// The daemon already spawned the sibling shell session.
	inst := startedMockInstance(t, agentName, agentName+"__shell")

	tab, err := inst.AttachShellTab("shell", "", "")
	require.NoError(t, err)
	assert.Equal(t, "shell", tab.Name)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount(), "the reconnected tab must be appended")
	assert.True(t, inst.TabAlive(1), "the reconnected tab must be bound to the live session")
	assert.Equal(t, agentName+"__shell", tab.tmux.SanitizedName(),
		"the tab must bind to the exact daemon-derived session name")

	// A second call for the same name is a no-op returning the existing tab —
	// guards against a refresh racing ahead of the reconnect.
	again, err := inst.AttachShellTab("shell", "", "")
	require.NoError(t, err)
	assert.Same(t, tab, again, "a duplicate attach must return the existing tab")
	assert.Equal(t, 2, inst.TabCount(), "a duplicate attach must not append again")
}

// TestAttachShellTab_RejectedForUnstarted verifies AttachShellTab errors when the
// instance has no live agent session/worktree.
func TestAttachShellTab_RejectedForUnstarted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := NewInstance(InstanceOptions{Title: "unstarted", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	_, err = inst.AttachShellTab("shell", "", "")
	require.Error(t, err)
	require.Equal(t, 0, inst.TabCount())
}

// TestDropClosedTab_RemovesWithoutKilling verifies the no-kill removal path
// (#960 PR 2): DropClosedTab removes a tab from the list but does NOT tear down
// its tmux session (the daemon already did), and still protects the agent tab.
func TestDropClosedTab_RemovesWithoutKilling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_drop")
	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount())
	require.True(t, tab.tmux.ExistsOrUnknown())

	require.Error(t, inst.DropClosedTab(0), "the agent tab must be undroppable")
	require.Equal(t, 2, inst.TabCount(), "a rejected drop must not mutate the list")
	require.Error(t, inst.DropClosedTab(9), "an out-of-range index must be rejected")

	require.NoError(t, inst.DropClosedTab(1))
	require.Equal(t, 1, inst.TabCount(), "drop must remove the tab from the list")
	require.True(t, tab.tmux.ExistsOrUnknown(),
		"DropClosedTab must NOT kill the tmux session (the daemon owns the kill)")
}

// TestUniqueShellName covers the per-instance naming sequence in isolation.
func TestUniqueShellName(t *testing.T) {
	assert.Equal(t, "shell", uniqueShellName(nil))
	assert.Equal(t, "shell-2", uniqueShellName([]*Tab{{Name: "shell"}}))
	assert.Equal(t, "shell-3", uniqueShellName([]*Tab{{Name: "shell"}, {Name: "shell-2"}}))
	// A hole in the sequence is filled with the lowest free name.
	assert.Equal(t, "shell-2", uniqueShellName([]*Tab{{Name: "shell"}, {Name: "shell-3"}}))
}

// TestRestartSurvival_HumanCreatedShellTab is the PR 4 restart-survival test: a
// shell tab created by the new-tab hotkey must persist through Storage and
// reconnect to its exact tmux session across an af/daemon restart, exactly like
// the default agent+shell pair (PR 2). Builds a started instance, adds an extra
// shell tab, round-trips it through Storage, and asserts all three tabs reload
// and reconnect.
func TestRestartSurvival_HumanCreatedShellTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_human_tab"
	shellName := agentName + "__shell"
	shell2Name := agentName + "__shell-2"

	inst := startedMockInstance(t, agentName)
	_, err := inst.AddShellTab() // shell
	require.NoError(t, err)
	third, err := inst.AddShellTab() // shell-2 — the "human-created" tab under test
	require.NoError(t, err)
	require.Equal(t, "shell-2", third.Name)
	require.Equal(t, 3, inst.TabCount())

	// Persist through Storage and reload with a fresh Storage, restoring sessions
	// for the exact persisted names so reconnection stays hermetic.
	repoID := config.RepoIDFromRoot(inst.Path)
	ms := newMockStorage()
	saveStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	require.NoError(t, saveStore.SaveInstances([]*Instance{inst}))

	restoreExec := nameKeyedExec(map[string]bool{agentName: true, shellName: true, shell2Name: true})
	restorePty := persistPtyFactory{t: t, cmdExec: restoreExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, restorePty, restoreExec)
	}
	defer func() { restoreTmuxSession = prev }()

	loadStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	loaded, err := loadStore.LoadInstances()
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	restored := loaded[0]
	tabs := restored.GetTabs()
	require.Len(t, tabs, 3, "all three tabs (agent + two shells) must survive a restart")
	assert.Equal(t, TabKindShell, tabs[2].Kind)
	assert.Equal(t, "shell-2", tabs[2].Name, "the human-created tab keeps its name")
	assert.Equal(t, shell2Name, tabs[2].tmux.SanitizedName(),
		"the human-created tab must reconnect to its exact persisted tmux session")
	assert.True(t, restored.TabAlive(2), "the restored human-created tab must be live")
}

// TestTmuxTeardownCount_CountsSessionsNotRosterEntries pins what the kill watchdog budgets
// against. teardownTabs kills exactly the entries whose tab.tmux is non-nil, so a
// web tab performs no per-tab teardown — and with the nine-tab cap gone, charging
// the whole roster would let a dozen iframe tabs push the wedge diagnostics out by
// ten minutes for work that never happens (#3023 review).
func TestTmuxTeardownCount_CountsSessionsNotRosterEntries(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tmux_count")
	_, err := inst.AddShellTab()
	require.NoError(t, err)
	n, ok := inst.TryTmuxTeardownCount()
	require.True(t, ok)
	require.Equal(t, 2, n, "agent + shell both own tmux sessions")

	_, err = inst.AddWebTab("http://localhost:5173", "")
	require.NoError(t, err)
	require.Equal(t, 3, inst.TabCount(), "the web tab is on the roster")
	n, ok = inst.TryTmuxTeardownCount()
	require.True(t, ok)
	require.Equal(t, 2, n, "but it owns no tmux session, so it costs no teardown")

	// A tab closed without a confirmed kill leaves a pending handle, and
	// teardownTabs appends every one of them to the SAME sequential close loop —
	// so they are teardown work even though they are off the roster. Ignoring them
	// under-budgets the watchdog and fires it on a healthy teardown.
	inst.mu.Lock()
	inst.pendingTabCleanup = append(inst.pendingTabCleanup, TabCleanupData{TabID: "t9", TmuxName: "af_x__stuck"})
	inst.mu.Unlock()
	n, ok = inst.TryTmuxTeardownCount()
	require.True(t, ok)
	require.Equal(t, 3, n, "a pending cleanup handle is one more tmux session the teardown must close")
	require.Equal(t, 3, inst.TabCount(), "and it is not on the roster")

	// Held lock: the budget must degrade rather than wait, or arming the kill
	// watchdog would block on the very wedge it exists to diagnose.
	inst.mu.Lock()
	_, ok = inst.TryTmuxTeardownCount()
	inst.mu.Unlock()
	require.False(t, ok, "a locked roster must report unavailable, never block")
}

// TestTabKindAllowances_ProjectsTheDaemonsOwnVerdict pins the contract the web UI
// consumes (#3060). The point is not the values — those are RefuseTabKind's and
// will change as #3062/#3054 lift refusals — but that the projection IS
// RefuseTabKind, so an affordance and the call behind it cannot drift. Web uses
// the representative external HTTPS target that answers the menu-level "can
// this kind work at all?" question; submission re-asks with the actual URL.
func TestTabKindAllowances_ProjectsTheDaemonsOwnVerdict(t *testing.T) {
	for _, workspace := range []WorkspaceKind{WorkspaceLocalWorktree, WorkspaceRemote} {
		caps := Capabilities{Workspace: workspace}
		allowances := tabKindAllowances(caps)
		require.NotEmpty(t, allowances)

		seen := map[string]bool{}
		for _, a := range allowances {
			seen[a.Kind] = true
			kind, ok := ParseTabKindName(a.Kind)
			require.Truef(t, ok, "%q must be a name the CLI accepts", a.Kind)

			// The assertion that matters: each entry equals what RefuseTabKind says now.
			target := ""
			if kind == TabKindWeb {
				target = tabKindAllowanceExternalWebTarget
			}
			err := caps.RefuseTabKind(kind, target)
			require.Equalf(t, err == nil, a.Allowed, "kind %q disagrees with RefuseTabKind", a.Kind)
			if err != nil {
				require.Equalf(t, err.Error(), a.Reason,
					"the daemon's own refusal text must be carried, not a client-invented one (%q)", a.Kind)
			} else {
				require.Emptyf(t, a.Reason, "an allowed kind carries no reason (%q)", a.Kind)
			}
		}
		require.False(t, seen["agent"],
			"the agent tab is not creatable, so offering it would be a control with no call behind it")
	}
}

// The projection is derived from the backend on every snapshot, so persisting it
// would store a stale answer — and it carries long, versioned refusal prose that
// an older binary would read back as fact.
func TestTabKindProjection_IsNotPersisted(t *testing.T) {
	mutable := true
	data := InstanceData{
		Title:            "s",
		TabKinds:         []TabKindAllowance{{Kind: "web", Allowed: false, Reason: "some long versioned reason"}},
		TabRosterMutable: &mutable,
	}
	stored := data.ForStorage()
	require.Nil(t, stored.TabKinds, "a derived verdict must not reach instances.json")
	require.Nil(t, stored.TabRosterMutable, "and the roster verdict is not persisted either")
	require.Equal(t, "s", stored.Title, "and the real record survives")
}

// Every projected Kind must be SUBMIT-READY — a value a client can put straight
// into CreateTabRequest.Kind. "process" is deliberately absent: it has no --kind
// spelling, so projecting it would hand clients an identifier ParseTabKindName
// rejects. Nothing is under-reported, because shell and process classify
// identically and a client reads the shell entry for both.
func TestTabKindAllowances_ProjectsOnlySubmittableKinds(t *testing.T) {
	kinds := map[string]bool{}
	for _, a := range tabKindAllowances(Capabilities{Workspace: WorkspaceLocalWorktree}) {
		_, ok := ParseTabKindName(a.Kind)
		require.Truef(t, ok, "%q must be accepted by ParseTabKindName, or a client cannot submit it", a.Kind)
		kinds[a.Kind] = true
	}
	for _, want := range []string{"shell", "web", "vscode"} {
		require.Truef(t, kinds[want], "every submittable kind must be projected; %q was missing", want)
	}
	require.False(t, kinds["process"], "process has no --kind spelling and must not be offered as one")

	// The property that makes the omission safe rather than convenient.
	require.Equal(t, TabKindRequires(TabKindShell), TabKindRequires(TabKindProcess),
		"shell and process must classify identically, or reading the shell entry for a process tab is wrong")
}

// TestTabRosterMutable_FalseSurvivesSerialization is the forward-compatibility
// case this projection exists for (#3062): a backend that allows a metadata-only
// kind while keeping TabManagement false. With a plain bool, omitempty erased that
// verdict, and a client cannot tell "the daemon said no" from "the daemon is too
// old to say" — so it falls back to the create verdict, which is TRUE there, and
// offers a rename tabMutationTarget rejects.
func TestTabRosterMutable_FalseSurvivesSerialization(t *testing.T) {
	no := false
	encoded, err := json.Marshal(InstanceData{Title: "s", TabRosterMutable: &no})
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"tab_roster_mutable":false`,
		"a false verdict must reach the client; omitempty on a plain bool erased it")

	var back InstanceData
	require.NoError(t, json.Unmarshal(encoded, &back))
	require.NotNil(t, back.TabRosterMutable, "and it must decode as PRESENT, not as a legacy absence")
	require.False(t, *back.TabRosterMutable)

	// An unprojected record stays absent, which is what a pre-#3060 daemon sends.
	absent, err := json.Marshal(InstanceData{Title: "s"})
	require.NoError(t, err)
	require.NotContains(t, string(absent), "tab_roster_mutable")
}

// TestSandboxRestore_KeepsMetadataTabsBehindTheAgentTab is #3062's restore half,
// and it pins BOTH halves of what went wrong the first time this was attempted.
//
//   - The web tab must come back at all: dropping it made off-box admission
//     non-durable, since the tab vanished at the next daemon restart with nothing
//     erroring.
//   - It must come back BEHIND the agent tab. remoteAgentBackend.Launch seeds an
//     agent tab only when the roster is empty, so restoring into Tabs at load time
//     stopped it seeding and left a web tab at index 0 — the slot that is
//     unclosable and that the PTY stream targets, so every consumer would read it
//     as the agent.
//
// The drain is exercised directly rather than through Launch: a restored sandbox
// row loads an INERT backend whose Launch refuses ("not provisioned"), because the
// live runtime is re-provisioned on restore rather than reconstructed here. Launch
// is the caller; this pins what it calls.
func TestSandboxRestore_KeepsMetadataTabsBehindTheAgentTab(t *testing.T) {
	data := InstanceData{
		Title:       "off-box",
		BackendType: "ssh",
		Tabs: []TabData{
			{ID: "t0", Name: "agent", Kind: TabKindAgent},
			{ID: "t1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/app"},
			// A PTY tab's workspace did not survive; it stays dropped, or the roster
			// gains an entry every later operation fails on.
			{ID: "t2", Name: "shell", Kind: TabKindShell, TmuxName: "af_x__shell"},
		},
	}
	instance, err := FromInstanceData(data)
	require.NoError(t, err)

	// Staged, NOT written into Tabs: that is what preserves the agent's slot.
	require.Empty(t, instance.Tabs, "restored metadata tabs must not occupy the roster before the agent tab exists")
	require.Len(t, instance.pendingMetadataTabs, 1, "only the metadata-only tab is staged")
	require.Equal(t, "docs", instance.pendingMetadataTabs[0].Name)

	// What Launch does: seed the agent tab, then drain.
	instance.Tabs = []*Tab{newRemoteAgentTab()}
	instance.appendPendingMetadataTabsLocked()

	require.Len(t, instance.Tabs, 2)
	require.Equal(t, TabKindAgent, instance.Tabs[0].Kind,
		"index 0 must be the AGENT tab — it is unclosable and the PTY stream targets it")
	web := instance.Tabs[1]
	require.Equal(t, TabKindWeb, web.Kind, "the web tab must survive the restart — it needs no worktree")
	require.Equal(t, "https://example.com/app", web.URL, "and its target with it, or the tab is empty")
	require.NotEmpty(t, web.ID, "restored tabs stay addressable by a stable id")

	// One-shot: a retried launch must not append the same tab twice.
	instance.appendPendingMetadataTabsLocked()
	require.Len(t, instance.Tabs, 2, "a retried launch must not duplicate the restored tab")
}

// A sandbox recovery that fails BEFORE Launch leaves restored rows staged and
// undrained. Both failure paths then persist whatever ToInstanceData returned, so
// serializing only i.Tabs would write an empty roster and lose the tab permanently
// — destroying the durability this restore exists to provide (#3062).
func TestToInstanceData_PersistsStagedTabsWhenRecoveryFailsBeforeLaunch(t *testing.T) {
	data := InstanceData{
		Title:       "off-box",
		BackendType: "ssh",
		Tabs: []TabData{
			{ID: "t0", Name: "agent", Kind: TabKindAgent},
			{ID: "t1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/app"},
		},
	}
	instance, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Empty(t, instance.Tabs, "premise: nothing is drained yet — this is the pre-Launch state")
	require.Len(t, instance.pendingMetadataTabs, 1)

	// What the failure paths persist.
	round := instance.ToInstanceData()

	// In PendingTabs, NOT Tabs: Tabs reserves index 0 for the agent, and a failed
	// recovery leaves it empty, so a staged row emitted there would take that slot.
	var web *TabData
	for i := range round.PendingTabs {
		if round.PendingTabs[i].Kind == TabKindWeb {
			web = &round.PendingTabs[i]
		}
	}
	require.NotNil(t, web, "a staged tab must survive a recovery that failed before Launch")
	require.Equal(t, "https://example.com/app", web.URL, "with its target, or it comes back empty")
	require.Equal(t, "t1", web.ID, "and its stable id, so it is the same tab and not a new one")
}

// A browser canonicalises legacy IPv4 forms before choosing the proxy path.
// Loopback forms must be refused, but the parser must not classify valid DNS
// names or numeric forms of external addresses as loopback (#3062).
func TestRefuseTabKind_RejectsBrowserCanonicalLoopbackShorthands(t *testing.T) {
	remote := Capabilities{Workspace: WorkspaceRemote}
	for _, target := range []string{
		"http://127.1:3000",
		"http://2130706433:3000",
		"http://0x7f000001:3000",
		"http://0177.1:3000",
		"http://127.0.0.1.:3000",
	} {
		require.Errorf(t, remote.RefuseTabKind(TabKindWeb, target),
			"%s is loopback to a browser and must not be admitted off-box", target)
	}
	for _, target := range []string{
		"https://example.com/app",
		"https://a1.de/app",
		"https://dead.beef/app",
		"https://134744072/app",  // browser-canonical 8.8.8.8
		"https://0x08080808/app", // browser-canonical 8.8.8.8
	} {
		require.NoError(t, remote.RefuseTabKind(TabKindWeb, target),
			"%s is external to a browser and must remain admissible off-box", target)
	}
}

// The URL Standard maps these three domain separators to an ASCII dot before
// browser navigation. Admission must make the same host canonicalization so a
// loopback tab cannot be persisted as an apparently external, unusable target.
func TestRefuseTabKind_RejectsBrowserUnicodeDotLoopback(t *testing.T) {
	remote := Capabilities{Workspace: WorkspaceRemote}
	for _, target := range []string{
		"http://127。0。0。1:3000",
		"http://127．0．0．1:3000",
		"http://127｡0｡0｡1:3000",
		"http://ｌｏｃａｌｈｏｓｔ:3000",
	} {
		normalized, err := NormalizeWebTabURL(target)
		require.NoError(t, err)
		require.Errorf(t, remote.RefuseTabKind(TabKindWeb, normalized),
			"%s is browser-canonical loopback and must not be admitted off-box", target)
	}
}

// Staged rows are persisted OUTSIDE the ordered Tabs list. A recovery that fails
// before Launch leaves Tabs empty, so emitting a staged web tab there would put it
// in the agent's index-0 slot — clients would render it as the unclosable agent,
// and daemon mutations would report it missing (#3062).
func TestToInstanceData_StagedRowsNeverOccupyTheAgentSlot(t *testing.T) {
	data := InstanceData{
		Title:       "off-box",
		BackendType: "ssh",
		Tabs: []TabData{
			{ID: "t0", Name: "agent", Kind: TabKindAgent},
			{ID: "t1", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/app"},
		},
	}
	instance, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Empty(t, instance.Tabs, "premise: the pre-Launch state, nothing drained")

	round := instance.ToInstanceData()
	require.Empty(t, round.Tabs, "no row may occupy the agent slot while the roster is empty")
	require.Len(t, round.PendingTabs, 1, "but the staged row is still persisted, or it is lost")
	require.Equal(t, "https://example.com/app", round.PendingTabs[0].URL)

	// And it survives a reload without being double-staged.
	again, err := FromInstanceData(round)
	require.NoError(t, err)
	require.Len(t, again.pendingMetadataTabs, 1, "restaging must not duplicate the row")
}

// An archived off-box session is inert, but its metadata-only web tabs are still
// useful restore placeholders in snapshots. The storage representation must keep
// those rows out of ordered Tabs: after a failed pre-Launch restore, folding a
// pending web row into Tabs would put it in the agent's index-0 slot.
func TestToInstanceData_ArchivedRemoteProjectsMetadataTabsWithoutPersistingThemInRoster(t *testing.T) {
	data := InstanceData{
		Title:       "archived-off-box",
		BackendType: "ssh",
		Status:      Archived,
		Liveness:    LiveArchived,
		Tabs: []TabData{
			{ID: "agent-id", Name: "agent", Kind: TabKindAgent},
			{ID: "web-id", Name: "docs", Kind: TabKindWeb, URL: "https://example.com/docs"},
		},
	}
	instance, err := FromInstanceData(data)
	require.NoError(t, err)

	projection := instance.ToInstanceData()
	require.Len(t, projection.Tabs, 2, "archived snapshots must retain the inert web placeholder")
	require.Equal(t, TabKindAgent, projection.Tabs[0].Kind, "the synthetic agent row keeps the ordering contract")
	require.Equal(t, "web-id", projection.Tabs[1].ID)
	require.Equal(t, "https://example.com/docs", projection.Tabs[1].URL)

	stored := projection.ForStorage()
	require.Empty(t, stored.Tabs,
		"the snapshot-only roster must not fold staged rows back into ordered durable Tabs")
}

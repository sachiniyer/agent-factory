package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2628 suite. The root agent's heal replaces its session record rather
// than re-spawning into it, and a fresh create comes up with only its agent tab
// (#1100) — so every terminal, process, web, and editor tab a root had was
// dropped by a tmux outage, silently from the record's point of view and very
// visibly from the user's (a one-row tab strip).

// carriedRoster is the roster a reaped root hands its replacement: the agent tab
// the create will spawn for itself, plus one of every kind a user can have open.
func carriedRoster(agentName string) []TabData {
	return []TabData{
		{ID: "tab-agent", Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
		{ID: "tab-shell", Name: shellTabName, Kind: TabKindShell, TmuxName: agentName + shellTmuxSuffix},
		{ID: "tab-logs", Name: "logs", Kind: TabKindProcess, Command: "tail -f log.txt", TmuxName: agentName + tmuxTabSeparator + "logs"},
		{ID: "tab-web", Name: "web", Kind: TabKindWeb, URL: "http://localhost:5173/"},
		{ID: "tab-vscode", Name: "vscode", Kind: TabKindVSCode},
	}
}

// carryTestRepo initializes a committed git repo a first-time local start can
// build a worktree from.
func carryTestRepo(t *testing.T) string {
	t.Helper()
	workdir := t.TempDir()
	runGit(t, workdir, "init")
	runGit(t, workdir, "config", "--local", "user.email", "test@example.com")
	runGit(t, workdir, "config", "--local", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "f.txt"), []byte("x"), 0644))
	runGit(t, workdir, "add", ".")
	runGit(t, workdir, "commit", "-m", "init")
	return workdir
}

// TestStartRestoresCarriedTabs is the #2628 headline assertion: a create handed
// a reaped record's roster must come up with those tabs, not with the agent tab
// alone. It goes through the production path (NewInstance -> Start(true) ->
// restoreCarriedTabs -> setupTabs) with a mock tmux, and checks the two halves
// separately — the roster the tab strip renders, and the tmux sessions behind
// the tabs that own one.
func TestStartRestoresCarriedTabs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	workdir := carryTestRepo(t)
	const agentName = "af_2628_root"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}

	inst, err := NewInstance(InstanceOptions{
		Title:       "root",
		Path:        workdir,
		Program:     "bash",
		InPlace:     true,
		RestoreTabs: carriedRoster(agentName),
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	tabs := inst.GetTabs()
	require.Len(t, tabs, 5, "every non-agent tab the reaped root had must come back")

	assert.Equal(t, TabKindAgent, tabs[0].Kind, "the launch spawns its own agent tab, at index 0")
	assert.NotEqual(t, "tab-agent", tabs[0].ID,
		"the carried agent tab must not displace the one this launch just spawned")

	assert.Equal(t, []string{shellTabName, "logs", "web", "vscode"},
		[]string{tabs[1].Name, tabs[2].Name, tabs[3].Name, tabs[4].Name},
		"names and order must survive the record swap")
	assert.Equal(t, []string{"tab-shell", "tab-logs", "tab-web", "tab-vscode"},
		[]string{tabs[1].ID, tabs[2].ID, tabs[3].ID, tabs[4].ID},
		"stable tab ids carry, so a client reconciles the same tabs rather than strangers")
	assert.Equal(t, "tail -f log.txt", tabs[2].Command)
	assert.Equal(t, "http://localhost:5173/", tabs[3].URL, "a web tab is only its target; losing the URL loses the tab")

	assert.True(t, inst.TabAlive(1), "the carried terminal tab must come back on a live tmux session")
	assert.True(t, inst.TabAlive(2), "the carried process tab must come back on a live tmux session")
	assert.Equal(t, agentName+shellTmuxSuffix, tabs[1].tmux.SanitizedName(),
		"the replacement root derives the same agent name, so the tab keeps its own tmux session name")
	assert.Equal(t, agentName+tmuxTabSeparator+"logs", tabs[2].tmux.SanitizedName())
	assert.Nil(t, tabs[3].tmux, "a web tab has no PTY")
	assert.Nil(t, tabs[4].tmux, "a vscode tab has no PTY")
}

// TestStartWithoutCarriedTabsIsUnchanged pins the #1100 default: an ordinary
// create carries no roster and must still come up with only its agent tab. The
// carry is one caller's opt-in, not a new default.
func TestStartWithoutCarriedTabsIsUnchanged(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	workdir := carryTestRepo(t)
	const agentName = "af_2628_plain"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}

	inst, err := NewInstance(InstanceOptions{Title: "plain-2628", Path: workdir, Program: "bash"})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	require.Len(t, inst.GetTabs(), 1, "a create with no carried roster keeps the #1100 agent-only default")
}

// TestRestoreCarriedTabsIsOneShot: the roster is consumed, not remembered. A
// second launch of the same object (a retried start) must not append the same
// tabs again — duplicated names and a second spawn on each tab's tmux name.
func TestRestoreCarriedTabsIsOneShot(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_2628_once"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	inst := &Instance{
		Title:       "once-2628",
		Path:        t.TempDir(),
		Program:     "bash",
		backend:     &LocalBackend{},
		carriedTabs: carriedRoster(agentName),
	}
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))

	inst.restoreCarriedTabs()
	require.Len(t, inst.GetTabs(), 5)

	inst.restoreCarriedTabs()
	require.Len(t, inst.GetTabs(), 5, "a second pass must not rebuild a roster that was already consumed")
}

// TestRestoreCarriedTabsRepairsCollidingRecords: the roster is data read off
// disk, so it can name the same tmux session twice (a hand-edited or truncated
// record). Reusing a name is what makes the carry free — the reap just killed
// those sessions — but reusing ONE name twice would have the second tab's Start
// refuse an existing session and leave the user a dead row. Re-derive instead.
func TestRestoreCarriedTabsRepairsCollidingRecords(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_2628_dupe"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	inst := &Instance{
		Title:   "dupe-2628",
		Path:    t.TempDir(),
		Program: "bash",
		backend: &LocalBackend{},
		carriedTabs: []TabData{
			{ID: "tab-agent", Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{ID: "tab-a", Name: shellTabName, Kind: TabKindShell, TmuxName: agentName + shellTmuxSuffix},
			{ID: "tab-b", Name: shellTabName, Kind: TabKindShell, TmuxName: agentName + shellTmuxSuffix},
			{ID: "tab-c", Name: "", Kind: TabKindShell, TmuxName: ""},
		},
	}
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))

	inst.restoreCarriedTabs()
	tabs := inst.GetTabs()
	require.Len(t, tabs, 4)
	assert.Equal(t, []string{shellTabName, "shell-2", "shell-3"},
		[]string{tabs[1].Name, tabs[2].Name, tabs[3].Name},
		"a duplicate or empty recorded name must be repaired the way a fresh spawn would name it")
	assert.NotEqual(t, tabs[1].tmux.SanitizedName(), tabs[2].tmux.SanitizedName(),
		"two tabs must never be handed the same tmux session name")
	assert.NotEmpty(t, tabs[3].ID, "a row with no recorded id is backfilled, never left unaddressable")
	assert.NotEmpty(t, tabs[3].tmux.SanitizedName())
}

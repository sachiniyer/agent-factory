package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The W4.3 stable-id create surface (#5571): the agent-server's tab routes
// delegate to the *WithID forms, whose contract is — a daemon-supplied id
// assigned verbatim, a fresh mint on empty, and the supplied id acting as the
// create's idempotency key (a replayed create adopts the tab the first call
// made; a same-id request of a different shape refuses). These pin that
// contract against the REAL admission path, so the wire can never adopt a
// looser one.

func TestAddShellTabWithID_VerbatimIDAndRetryDedupe(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_id_shell")

	tab, err := inst.AddShellTabWithID("daemon-tab-1")
	require.NoError(t, err)
	assert.Equal(t, "daemon-tab-1", tab.ID, "the daemon's stable id is assigned verbatim")
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the created tab is live")

	// A replayed create — the reconnect-retry shape the daemon's idempotency
	// contract exists for — answers the tab the first call made and never lands
	// a second one under the id.
	retry, err := inst.AddShellTabWithID("daemon-tab-1")
	require.NoError(t, err)
	assert.Same(t, tab, retry, "a same-id same-kind retry adopts the existing tab")
	assert.Equal(t, 2, inst.TabCount(), "the retry must not append")
	assert.True(t, inst.TabAlive(1), "the adopted tab is still live")
}

func TestAddShellTabWithID_EmptyIDMints(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_id_mint")

	tab, err := inst.AddShellTabWithID("")
	require.NoError(t, err)
	assert.NotEmpty(t, tab.ID, "an empty supplied id mints exactly as AddShellTab does")
	assert.Equal(t, 2, inst.TabCount())
}

func TestAddProcessTabWithID_DedupeAndCollisions(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_id_proc")

	proc, err := inst.AddProcessTabWithID("proc-1", "btop --tree", "monitor")
	require.NoError(t, err)
	assert.Equal(t, "proc-1", proc.ID)
	assert.Equal(t, TabKindProcess, proc.Kind)
	assert.Equal(t, "monitor", proc.Name)

	// Same id + same kind + same command is a replay — the id names THIS tab.
	retry, err := inst.AddProcessTabWithID("proc-1", "btop --tree", "monitor")
	require.NoError(t, err)
	assert.Same(t, proc, retry)
	assert.Equal(t, 2, inst.TabCount())

	// Same id, different command: the command is part of what the id names, so
	// this is a collision, not a replay.
	_, err = inst.AddProcessTabWithID("proc-1", "htop", "")
	require.Error(t, err, "a same-id create with a different command is a collision, not a replay")
	assert.Equal(t, 2, inst.TabCount())

	// Same id, different kind: likewise refused — an id never changes shape.
	_, err = inst.AddShellTabWithID("proc-1")
	require.Error(t, err, "a same-id shell create against a process tab is a collision")
	assert.Equal(t, 2, inst.TabCount())

	// A DIFFERENT id on the same command is fine — ids namespace tabs, not shapes.
	other, err := inst.AddProcessTabWithID("proc-2", "btop --tree", "")
	require.NoError(t, err)
	assert.Equal(t, "proc-2", other.ID)
	assert.Equal(t, 3, inst.TabCount())
}

// Two concurrent same-id creates can both pass the early check and both spawn —
// the dedupe must hold at the append point too: whoever reaches the write lock
// second closes its orphan and adopts the winner's tab. The outcome is
// deterministic even though the path is racy: exactly one tab under the id,
// answered to both callers.
func TestAddShellTabWithID_ConcurrentSameIDLandsOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_id_race")

	var wg sync.WaitGroup
	results := make(chan *Tab, 2)
	errs := make(chan error, 2)
	for k := 0; k < 2; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tab, err := inst.AddShellTabWithID("raced-id")
			results <- tab
			errs <- err
		}()
	}
	wg.Wait()
	t1, e1 := <-results, <-errs
	t2, e2 := <-results, <-errs
	require.NoError(t, e1)
	require.NoError(t, e2)
	assert.Same(t, t1, t2, "both creates must answer the one tab that landed")
	assert.Equal(t, 2, inst.TabCount(), "exactly one tab landed under the raced id")
	assert.True(t, inst.TabAlive(1), "the surviving tab's tmux session is the live one")
}

// A concurrent same-id create must never spawn its own command: the roster
// dedupe runs after spawn, and for a process tab "spawned and reaped" is
// already a duplicated deploy. tabCreateInFlight serializes the id instead —
// the joiner waits on the owner's outcome and adopts, so exactly one
// new-session ever runs under the id. Mismatched same-id requests refuse
// before spawning too.
func TestAddProcessTabWithID_ConcurrentSameIDJoinsWithoutSpawning(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tabs_id_join"
	// The FIRST tmux new-session blocks until the test releases it — holding a
	// supplied-id create provably in-flight so a second same-id request's
	// behavior is observable. newSessions records how many new-session calls
	// ran, which is what proves the joiner never spawned.
	inner := nameKeyedExec(map[string]bool{agentName: true})
	var newSessions atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	block := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "new-session") && newSessions.Add(1) == 1 {
				close(entered)
				<-release
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.OutputFunc,
	}
	pty := persistPtyFactory{t: t, cmdExec: block}
	repoPath := "/tmp/tab-lifecycle-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, existingDir(t, filepath.Join(t.TempDir(), "wt")), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title: agentName, Path: repoPath, Program: "claude",
		backend: &LocalBackend{}, started: true, gitWorktree: gw,
		Tabs: []*Tab{newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, block))},
	}

	// Create 1 spawns and stalls inside new-session — provably in-flight once
	// `entered` closes (the id claim precedes the spawn, so it is held by then).
	first := make(chan *Tab, 1)
	firstErr := make(chan error, 1)
	go func() {
		tab, err := inst.AddProcessTabWithID("dup", "deploy.sh", "")
		first <- tab
		firstErr <- err
	}()
	<-entered

	// Same id, same shape: joins — spawns nothing, answers once the owner
	// resolves.
	joined := make(chan *Tab, 1)
	joinedErr := make(chan error, 1)
	go func() {
		tab, err := inst.AddProcessTabWithID("dup", "deploy.sh", "")
		joined <- tab
		joinedErr <- err
	}()

	// Same id, different shape: refuses BEFORE spawning — answered while
	// create 1 is still blocked, so a return at all proves the early refusal.
	if _, err := inst.AddProcessTabWithID("dup", "other.sh", ""); err == nil {
		t.Fatal("a same-id create with a different command must refuse while the first is in flight")
	}
	if _, err := inst.AddShellTabWithID("dup"); err == nil {
		t.Fatal("a same-id shell create against an in-flight process create must refuse")
	}

	close(release)
	winner, jt := <-first, <-joined
	require.NoError(t, <-firstErr)
	require.NoError(t, <-joinedErr)
	assert.Same(t, winner, jt, "the joiner adopts the tab the first create made")
	assert.Equal(t, 2, inst.TabCount(), "exactly one tab landed under the raced id")
	assert.Equal(t, int32(1), newSessions.Load(), "the joiner never ran new-session")
}

// A staged pendingMetadataTabs row owns its id too: a live-roster id and a
// staged id are the same authority, so a create colliding with the staged row
// refuses rather than producing a duplicate id when the stage drains.
func TestAddShellTabWithID_StagedIDRefuses(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_id_staged")
	inst.pendingMetadataTabs = []TabData{{ID: "staged-1", Name: "docs", Kind: TabKindWeb}}

	_, err := inst.AddShellTabWithID("staged-1")
	require.Error(t, err, "a supplied id that a staged row already owns is a collision")
	assert.Equal(t, 1, inst.TabCount(), "nothing lands under a staged id")
}

// The WithID forms are the SAME admission path as the plain ones: an instance
// whose spawn gate refuses keeps refusing — no second, looser wire path.
func TestAddTabWithID_UnstartedRefuses(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "unstarted", Path: t.TempDir()})
	require.NoError(t, err)

	if _, err := inst.AddShellTabWithID("x"); err == nil {
		t.Fatal("AddShellTabWithID on an unstarted instance: want refusal, got nil")
	}
	if _, err := inst.AddProcessTabWithID("x", "btop", ""); err == nil {
		t.Fatal("AddProcessTabWithID on an unstarted instance: want refusal, got nil")
	}
}

// TabsData is the roster the agent-server reports: every tab with its stable
// id, kind, and name — and, for a process tab, the exit evidence
// RecordProcessTabExits just refreshed. Without the refresh a command that
// finished while the sandbox ran would report a stale un-stamped row forever:
// nothing on a LIVE instance ever looks, so the roster read is where the look
// happens.
func TestTabsData_ReportsRosterAndRefreshedExitEvidence(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tabs_roster"
	procName := agentName + tmuxTabSeparator + "deploy"
	// The mock answers the deploy session as existing AND holding a finished
	// pane (status 7, died at 1726000000), the way tmux answers a held dead
	// pane.
	exec := nameKeyedExecWithFinishedPanes(
		map[string]bool{agentName: true, procName: true},
		map[string]finishedPane{procName: {status: "7", at: "1726000000", start: afPaneStart("./deploy.sh")}},
	)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	repoPath := "/tmp/tabs-roster-repo"
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, existingDir(t, filepath.Join(t.TempDir(), "wt")), "roster",
		"roster-branch", "", false, true)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "roster",
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs: []*Tab{
			newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)),
			{ID: "deploy-id", Name: "deploy", Kind: TabKindProcess, Command: "./deploy.sh",
				tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(procName, "./deploy.sh", pty, exec)},
		},
	}

	before := inst.TabsData()
	require.Len(t, before, 2)
	assert.Equal(t, TabKindAgent, before[0].Kind, "the agent row rides at index 0")
	assert.Equal(t, "deploy-id", before[1].ID, "the roster reports stable ids")
	assert.Equal(t, TabKindProcess, before[1].Kind)
	assert.Equal(t, "./deploy.sh", before[1].Command)
	assert.Nil(t, before[1].Exit, "the roster reports no exit until something looks")

	inst.RecordProcessTabExits()

	after := inst.TabsData()
	require.Len(t, after, 2)
	require.NotNil(t, after[1].Exit, "the refreshed roster row carries the exit evidence")
	assert.Equal(t, 7, after[1].Exit.Status)
	assert.True(t, after[1].Exit.StatusKnown)
	assert.True(t, after[1].Exit.At.Equal(time.Unix(1726000000, 0)),
		"the pane's reported death time survives to the wire, at = %v", after[1].Exit.At)
}

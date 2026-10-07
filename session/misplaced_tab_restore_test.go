package session

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests cover the #5174 sibling-tab rule: a pane restored under a
// persisted tab name but PROVEN to sit outside the session's worktree is
// foreign — never bound, presented, attached, captured, or stamped — while the
// af-namespaced tmux name it squats on stays killable by teardown, exactly as
// the agent pane's refused-reattach keeps its name bound for kill-by-name.
//
// The mocks address tmux targets through tmuxTargetName (shared with
// backend_local_start_timeout_test.go) — exact-match is load-bearing:
// proc-restore__shell-2 contains proc-restore__shell as a substring, so a
// Contains check would answer the squatter's verdict for the fresh shell.

// misplacedPaneExec answers the tmux surface of setupTabs' tab restore:
// has-session per exact target, then the placement probes. pane_dead reports
// alive and pane_pid reports a pid that does not exist, so the procfs source
// is skipped on every query and pane_current_path is the convicting source.
//
// squatters is the set of tmux sessions that exist BEFORE af spawns anything —
// their panes all answer "/" as cwd, an existing directory never inside the
// fixture's temp worktree. placed maps session name -> the cwd tmux reports;
// it is captured by reference so a test can add an answer after constructing
// the instance (the fresh shell's name is only known once the roster exists).
// Existence is deliberately NOT read off placed: a session whose placement is
// merely ANSWERABLE must still come up through spawned first, or the mock
// would report a name as live before its new-session ever ran.
func misplacedPaneExec(squatters map[string]bool, placed map[string]string, spawned func(string) bool) cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "has-session") {
				return nil
			}
			name := tmuxTargetName(c.Args)
			if squatters[name] || (spawned != nil && spawned(name)) {
				return nil
			}
			return errors.New("can't find session")
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "pane_dead"):
				return []byte("0"), nil
			case strings.Contains(joined, "pane_pid"):
				return []byte("99999999"), nil
			case strings.Contains(joined, "pane_current_path"),
				strings.Contains(joined, "pane_start_path"):
				name := tmuxTargetName(c.Args)
				if squatters[name] {
					return []byte("/"), nil
				}
				if dir, ok := placed[name]; ok {
					return []byte(dir), nil
				}
			}
			return nil, nil
		},
	}
}

// shellFreshSpawned reports whether tmux's answer for a has-session on name
// should now be "exists": true once the pty factory has emitted that session's
// new-session — the recorded-spawn signal this package's other restore tests
// use, since new-session travels through the pty factory, not the executor.
func shellFreshSpawned(pty **recordingPtyFactory, fresh string) func(string) bool {
	return func(name string) bool {
		if name != fresh || *pty == nil {
			return false
		}
		for _, c := range (*pty).cmds {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "new-session") && strings.Contains(joined, fresh) {
				return true
			}
		}
		return false
	}
}

// A process tab whose persisted name is held by a pane outside the worktree:
// the reattach refuses it, the tab goes inert (its command is never re-run),
// the foreign binding leaves the roster so nothing can attach or capture it,
// and the squatted name lands on the durable cleanup list so teardown still
// kills it — the same one rule the agent pane's refusal follows (#5174).
func TestProcessTabRestoreMisplacedPaneIsQuarantined(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// The squatter's pane answers "/" — an existing directory never inside the
	// fixture's temp worktree — which convicts it through pane_current_path.
	cmdExec := misplacedPaneExec(map[string]bool{"proc-restore-proc": true}, nil, nil)
	inst, procTab, pty := processTabRestoreInstance(t, cmdExec)

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))

	tab := inst.Tabs[1]
	assert.True(t, tab.inert, "a tab bound to a foreign pane must come back inert — nothing may re-run or present it")
	assert.Nil(t, tab.tmux, "the roster must not keep a handle to a pane proven misplaced")
	assert.Nil(t, tab.Exit, "a foreign pane's exit state is never this command's evidence")
	assert.Zero(t, newSessionCount(pty), "a process command is never re-executed")
	assert.Equal(t, []TabCleanupData{{TabID: procTab.ID, TmuxName: "proc-restore-proc"}},
		inst.PendingTabCleanup(),
		"the squatted af-namespaced session must stay killable by teardown")
}

// A shell tab in the same shape is replaced rather than inerted — a terminal is
// interruption-to-repair — but under a FRESH token: the squatted name is never
// retaken, the roster ends bound to a pane verified inside the worktree, and
// the squatter goes to pendingTabCleanup for teardown to kill (#5174).
func TestShellTabRestoreMisplacedPaneRespawnsFreshAndKeepsSquatterKillable(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// The agent session is af_proc-restore (the constructor applies
	// the af_ prefix); siblings take verbatim names, so the tmux token
	// math below keys off the af_-prefixed prefix.
	const squatter = "af_proc-restore__shell"
	const fresh = "af_proc-restore__shell-2"

	var pty *recordingPtyFactory
	// The process tab answers "no such session" outright (absent -> inert, the
	// #4479 arm this file already covers), keeping this test about the shell.
	placed := map[string]string{}
	cmdExec := misplacedPaneExec(map[string]bool{squatter: true}, placed, shellFreshSpawned(&pty, fresh))
	inst, _, p := processTabRestoreInstance(t, cmdExec)
	pty = p
	placed[fresh] = inst.gitWorktree.GetWorktreePath()

	shellTmux, err := inst.Tabs[0].tmux.NewShellSiblingSession(squatter, "/bin/sh")
	require.NoError(t, err)
	inst.Tabs = append(inst.Tabs, &Tab{ID: "shell-tab", Name: "shell", Kind: TabKindShell, tmux: shellTmux})

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))

	tab := inst.Tabs[2]
	require.NotNil(t, tab.tmux, "the restored shell must be bound to a live session")
	assert.NotSame(t, shellTmux, tab.tmux, "the refused binding must be replaced, not kept")
	assert.Equal(t, fresh, tab.tmux.SanitizedName(),
		"the replacement must take a fresh token — the squatted name is never retaken")
	assert.False(t, tab.tmux.MisplacedPane(), "the fresh session's pane was verified inside the worktree")
	assert.Equal(t, []TabCleanupData{{TabID: "shell-tab", TmuxName: squatter}},
		inst.PendingTabCleanup(),
		"the squatter's name must remain teardown-killable after the roster drops it")
	assert.Positive(t, newSessionCount(pty), "the dead-shell path must have spawned the replacement")
}

// The other half of the one-rule contract (#5174 review): after a misplaced
// shell is quarantined and respawned, tearing the session down kills both tmux
// names — the foreign squatter via its durable cleanup handle and the fresh
// replacement via the roster binding. Neither survives; nothing leaks and the
// name is not left squatted.
func TestMisplacedShellRespawnTeardownKillsBothNames(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// The agent session is af_proc-restore (the constructor applies
	// the af_ prefix); siblings take verbatim names, so the tmux token
	// math below keys off the af_-prefixed prefix.
	const squatter = "af_proc-restore__shell"
	const fresh = "af_proc-restore__shell-2"

	var pty *recordingPtyFactory
	placed := map[string]string{}
	cmdExec := misplacedPaneExec(map[string]bool{squatter: true}, placed, shellFreshSpawned(&pty, fresh))
	inst, _, p := processTabRestoreInstance(t, cmdExec)
	pty = p
	placed[fresh] = inst.gitWorktree.GetWorktreePath()

	shellTmux, err := inst.Tabs[0].tmux.NewShellSiblingSession(squatter, "/bin/sh")
	require.NoError(t, err)
	inst.Tabs = append(inst.Tabs, &Tab{ID: "shell-tab", Name: "shell", Kind: TabKindShell, tmux: shellTmux})
	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	require.NotNil(t, inst.Tabs[2].tmux, "precondition: the fresh shell bound")

	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedName(name, program)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Kill-mode teardown records which tmux NAMES it reaps — the roster binding
	// and the pending-cleanup handle alike.
	var killed []string
	prevClose := killCloseTab
	killCloseTab = func(ts *tmux.TmuxSession, _, _ string) (teardownState, bool, error) {
		killed = append(killed, ts.SanitizedName())
		return stateKnown, false, nil
	}
	t.Cleanup(func() { killCloseTab = prevClose })

	// The worktree delete may report a non-settled state for a fixture worktree
	// that is not a real git registration; the assertions below are about which
	// panes teardown killed, which happens before that gate.
	_ = inst.teardownTabs(teardownKill{})

	assert.Contains(t, killed, squatter,
		"teardown must reap the squatted af-namespaced session via its pending handle")
	assert.Contains(t, killed, fresh,
		"the roster's fresh replacement is torn down by name like any live tab")
	assert.Empty(t, inst.PendingTabCleanup(), "a confirmed teardown retires the handle it reaped")
}

// A tab spawn while af's own worktree relocation is unresolved must refuse:
// the recorded path is not authoritative, so shell and process tabs get the
// same refusal agent restore, respawn, and swap already keep (#5174).
func TestAddTabRefusesSpawnDuringUnresolvedRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_reloc_tabs")
	require.NoError(t, inst.gitWorktree.RestoreRelocationRecovery(git.RelocationRecovery{
		State: git.RelocationRecoveryStalled,
	}))
	_, _, unresolved := inst.gitWorktree.RelocationSnapshot()
	require.True(t, unresolved, "precondition: the relocation is unresolved")

	tabsBefore := inst.TabCount()
	_, err := inst.AddShellTab()
	require.ErrorContains(t, err, "worktree relocation",
		"a shell tab must refuse to spawn while the worktree's path is not authoritative")
	_, err = inst.AddProcessTab("echo hi", "")
	require.ErrorContains(t, err, "worktree relocation",
		"a process tab must refuse the same way")
	assert.Equal(t, tabsBefore, inst.TabCount(), "a refused spawn appends no tab")
}

package session

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2669 review follow-up. Persisting the close before teardown fixed the
// resurrection half of the bug and opened its mirror image: teardown now runs
// AFTER the tab is durably gone, so a kill-session that times out (or answers
// while the session survives) used to drop the closed tab's only tmux identity.
// The process then leaked untracked forever, and a later tab with the same name
// re-derived the same tmux name and could not start at all.
//
// These cover the retention contract end to end: a handle is written when
// teardown is unconfirmed, retired when it is confirmed, retried across a
// restart, and reserved against by a spawn in the meantime — while never
// reappearing as a tab.

// cleanupExec models a tmux server where kill-session FAILS for the named
// sessions and leaves them alive, which is the exact shape close.go turns into
// an "error killing tmux session" (kill answered, the follow-up has-session
// probe still sees it). Every other session kills normally.
func cleanupExec(alive map[string]bool, unkillable map[string]bool) (cmd_test.MockCmdExec, func(string) bool, func() int) {
	var mu sync.Mutex
	existing := map[string]bool{}
	for name, ok := range alive {
		existing[name] = ok
	}
	kills := 0
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	mockExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := nameOf(cmd)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(s, "has-session"):
				if existing[name] {
					return nil
				}
				return errors.New("can't find session")
			case strings.Contains(s, "new-session"):
				existing[name] = true
			case strings.Contains(s, "kill-session"):
				kills++
				if unkillable[name] {
					// Answers with a failure and the session stays up: Close probes,
					// sees it, and reports a real teardown error rather than a timeout.
					return errors.New("kill-session refused")
				}
				delete(existing, name)
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	return mockExec, func(name string) bool {
			mu.Lock()
			defer mu.Unlock()
			return existing[name]
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return kills
		}
}

// cleanupInstanceData is a normal live local session with one closable shell tab.
func cleanupInstanceData(t *testing.T, agentName, shellName string) InstanceData {
	t.Helper()
	const repoPath = "/tmp/tab-cleanup-repo"
	return InstanceData{
		Title:    "tab-cleanup",
		Path:     repoPath,
		Program:  "claude",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{ID: "tab-agent", Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{ID: "tab-shell", Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(t.TempDir(), "wt"),
			SessionName:  "tab-cleanup",
			BranchName:   "tab-cleanup-branch",
		},
	}
}

// TestCloseTabByIDWithCommit_UnconfirmedTeardownRetainsCleanupHandle is the
// headline regression. The close commits, so the tab is durably gone — but the
// kill failed with the session still up, so the projection handed to commit must
// already carry a cleanup handle naming that session. Without it the persisted
// record is the last thing that ever mentioned the tmux name.
func TestCloseTabByIDWithCommit_UnconfirmedTeardownRetainsCleanupHandle(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tab-cleanup_agent"
	shellName := agentName + "__shell"
	mockExec, aliveFn, _ := cleanupExec(
		map[string]bool{agentName: true, shellName: true},
		map[string]bool{shellName: true})
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	defer func() { restoreTmuxSession = prev }()

	inst, err := FromInstanceData(cleanupInstanceData(t, agentName, shellName))
	require.NoError(t, err)

	var committed InstanceData
	result, err := inst.CloseTabByIDWithCommit("tab-shell", func(staged InstanceData) error {
		committed = staged
		return nil
	})
	require.NoError(t, err, "the close itself must succeed: only the teardown was unconfirmed")
	require.Error(t, result.TeardownErr, "the fixture's kill-session must fail; the test's premise is wrong")
	assert.Nil(t, result.Settled, "an unconfirmed teardown has nothing to settle")

	// The durable decision: the tab is gone from the roster...
	for _, td := range committed.Tabs {
		assert.NotEqual(t, shellName, td.TmuxName,
			"the committed roster still carries the closed tab; a restart would respawn it (#2669)")
	}
	// ...and its tmux session is still named by something durable.
	require.Len(t, committed.PendingTabCleanup, 1,
		"an unconfirmed teardown dropped the closed tab's only tmux identity: the session is now untracked forever")
	assert.Equal(t, shellName, committed.PendingTabCleanup[0].TmuxName)
	assert.Equal(t, "tab-shell", committed.PendingTabCleanup[0].TabID)

	// The handle is live state too, not just a one-shot projection: a later
	// snapshot (the daemon's checkpoint) must keep carrying it.
	assert.True(t, aliveFn(shellName), "fixture: the unkillable session must still be up")
	assert.Equal(t, []TabCleanupData{{TabID: "tab-shell", TmuxName: shellName}}, inst.PendingTabCleanup())
	assert.Len(t, inst.ToInstanceData().PendingTabCleanup, 1)
}

// TestCloseTabByIDWithCommit_ConfirmedTeardownRetiresCleanupHandle is the other
// half of the contract, and the one that keeps the mechanism from becoming a
// permanent leak of its own: the ordinary close must leave NO handle behind, so
// no daemon start ever retries a teardown that already finished.
func TestCloseTabByIDWithCommit_ConfirmedTeardownRetiresCleanupHandle(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tab-cleanup_agent"
	shellName := agentName + "__shell"
	mockExec, aliveFn, _ := cleanupExec(
		map[string]bool{agentName: true, shellName: true}, nil)
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	defer func() { restoreTmuxSession = prev }()

	inst, err := FromInstanceData(cleanupInstanceData(t, agentName, shellName))
	require.NoError(t, err)

	var committed InstanceData
	result, err := inst.CloseTabByIDWithCommit("tab-shell", func(staged InstanceData) error {
		committed = staged
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, result.TeardownErr)
	assert.False(t, aliveFn(shellName), "fixture: a confirmed close must actually kill the session")

	// The handle is staged for the commit — the write has to precede the kill, or
	// a crash between them loses the identity...
	require.Len(t, committed.PendingTabCleanup, 1,
		"the handle must be durable BEFORE teardown: a crash in between would lose the tmux identity")
	// ...and retired once the kill is confirmed, with a projection that says so.
	assert.Empty(t, inst.PendingTabCleanup(), "a confirmed teardown must retire its handle")
	require.NotNil(t, result.Settled, "a confirmed teardown must hand back the retirement to persist")
	assert.Empty(t, result.Settled.PendingTabCleanup)
}

// TestCloseTabByIDWithCommit_CommitFailureDropsStagedCleanupHandle guards the
// rollback. The tab is put back because it still owns its tmux session, so a
// handle for that same session must NOT survive: it would double-count a live
// tab as awaiting cleanup and let the startup sweep kill the session out from
// under it.
func TestCloseTabByIDWithCommit_CommitFailureDropsStagedCleanupHandle(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tab-cleanup_agent"
	shellName := agentName + "__shell"
	mockExec, aliveFn, _ := cleanupExec(
		map[string]bool{agentName: true, shellName: true}, nil)
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	defer func() { restoreTmuxSession = prev }()

	inst, err := FromInstanceData(cleanupInstanceData(t, agentName, shellName))
	require.NoError(t, err)

	_, err = inst.CloseTabByIDWithCommit("tab-shell", func(InstanceData) error {
		return errors.New("persist failed")
	})
	require.Error(t, err)

	assert.Equal(t, 2, inst.TabCount(), "the rollback must restore the tab")
	assert.True(t, aliveFn(shellName), "the rollback must leave the tab's session alive")
	assert.Empty(t, inst.PendingTabCleanup(),
		"a rolled-back close left a cleanup handle for a tab that is still on the roster; the sweep would kill its live session")
	assert.Empty(t, inst.ToInstanceData().PendingTabCleanup)
}

// TestRetryPendingTabCleanup_RetiresOnlyConfirmedKills is the across-restart
// half of the finding: a handle persisted by a previous daemon must be retried,
// and retired ONLY on a confirmed kill. tmux Close is idempotent (#967), so a
// nil error genuinely means no such session remains — which is the one answer
// that justifies dropping the last durable pointer to it. An unconfirmed retry
// keeps its handle for the next start.
func TestRetryPendingTabCleanup_RetiresOnlyConfirmedKills(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tab-cleanup_agent"
	reapable := agentName + "__gone"
	stuck := agentName + "__stuck"
	mockExec, aliveFn, killCount := cleanupExec(
		map[string]bool{agentName: true, reapable: true, stuck: true},
		map[string]bool{stuck: true})
	pty := persistPtyFactory{t: t, cmdExec: mockExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, mockExec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := cleanupInstanceData(t, agentName, agentName+"__shell")
	data.PendingTabCleanup = []TabCleanupData{
		{TabID: "closed-1", TmuxName: reapable},
		{TabID: "closed-2", TmuxName: stuck},
	}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)

	// A tombstone must never load as a tab: the whole point is that these are
	// CLOSED. Two tabs is the fixture's roster, unchanged by the two handles.
	assert.Equal(t, 2, inst.TabCount(), "a cleanup handle was restored as a tab; the closed tab is back")
	require.Len(t, inst.PendingTabCleanup(), 2, "both handles must survive the restart")

	before := killCount()
	retired, remaining := inst.RetryPendingTabCleanup()
	assert.Greater(t, killCount(), before, "the sweep must actually re-issue kill-session")
	assert.Equal(t, 1, retired)
	assert.Equal(t, 1, remaining)
	assert.False(t, aliveFn(reapable), "the reapable leftover session must be gone")
	assert.Equal(t, []TabCleanupData{{TabID: "closed-2", TmuxName: stuck}}, inst.PendingTabCleanup(),
		"only the confirmed kill may be retired; an unconfirmed one keeps its handle for the next start")
}

// TestUniqueTabTmuxName_ReservesPendingCleanupTokens covers the user-visible
// half of the finding. After an unconfirmed close of "fresh" nothing on the
// roster holds that token, so the next spawn re-derived the survivor's exact
// name and TmuxSession.Start rejected it as already existing — a wedge no retry
// clears, because every retry derives the same name. Reserving the pending token
// walks the spawn to "fresh-2" instead, exactly as it does around a renamed
// sibling (#1957).
func TestUniqueTabTmuxName_ReservesPendingCleanupTokens(t *testing.T) {
	const agentName = "af_tab-cleanup_agent"
	tabs := []*Tab{{Name: agentTabName, Kind: TabKindAgent}}

	assert.Equal(t, agentName+"__fresh", uniqueTabTmuxName(tabs, nil, agentName, "fresh"),
		"with nothing pending the spawn keeps the requested token")

	pending := []TabCleanupData{{TabID: "closed", TmuxName: agentName + "__fresh"}}
	assert.Equal(t, agentName+"__fresh-2", uniqueTabTmuxName(tabs, pending, agentName, "fresh"),
		"a spawn re-derived the tmux name of a session whose teardown was never confirmed; Start would reject it as already existing")

	// A handle from some other prefix reserves nothing here — the same honesty
	// tabTmuxToken applies rather than deriving a token it cannot trust.
	foreign := []TabCleanupData{{TabID: "closed", TmuxName: "af_other_agent__fresh"}}
	assert.Equal(t, agentName+"__fresh", uniqueTabTmuxName(tabs, foreign, agentName, "fresh"))
}

package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// archivableTabExec is a name-keyed tmux mock (agent seeded alive) that also
// reports whether a given session name currently exists, so a test can assert a
// process-tab session was NEVER spawned (no orphan) after a rejected CreateTab.
// OutputFunc returns non-numeric "content" so panePID fails to parse and
// CloseAndWaitForPaneExit treats the pane as already gone — no real wait.
func archivableTabExec(agentName string) (cmd_test.MockCmdExec, func(string) bool) {
	existing := map[string]bool{agentName: true}
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
	e := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return &tabNoSessionErr{}
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	return e, func(name string) bool { return existing[name] }
}

// registerArchivableWithTmux is registerArchivable plus a mock-backed tmux agent
// session, so CreateTab's AddProcessTab/AddShellTab path actually attempts a
// sibling spawn (the surface that would orphan). Returns the instance, the
// worktree's original path, and an alive-probe over modeled tmux session names.
func registerArchivableWithTmux(t *testing.T, m *Manager, repoID, repoPath, title, agentName string) (*session.Instance, string, func(string) bool) {
	t.Helper()
	wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+sanitizeArchiveTitle(title))
	branch := "af/" + sanitizeArchiveTitle(title)
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
	require.NoError(t, err, string(out))

	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
	require.NoError(t, err)

	e, isAlive := archivableTabExec(agentName)
	pty := tabPtyFactory{t: t, cmdExec: e}

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, e))
	inst.SetStartedForTest(true)
	inst.SetStatus(session.Ready)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst, wtPath, isAlive
}

// TestCreateTab_RejectedAfterArchive: once a session is archived (tmux torn down,
// worktree moved, status Archived), a CreateTab for it is rejected with an
// actionable message and spawns NO tmux session — there is no worktree at the old
// path to orphan into (#1195).
func TestCreateTab_RejectedAfterArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	const title, agentName = "worker", "af_worker_agent"
	_, srcPath, isAlive := registerArchivableWithTmux(t, manager, repoID, repoPath, title, agentName)

	_, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.False(t, exists(srcPath), "archive must have moved the worktree out of its original path")

	_, err = manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Command: "btop"})
	require.Error(t, err, "CreateTab on an archived session must be rejected")
	assert.Contains(t, err.Error(), "archived")
	assert.False(t, isAlive(agentName+"__btop"), "no process tmux session may be spawned into the moved-away worktree")
}

// TestCreateTab_SerializedWithInFlightArchiveDoesNotOrphan: a CreateTab arriving
// while an ArchiveSession holds the per-session op-lock (mid teardown+move) must
// not spawn a tmux session into the worktree being relocated. Because CreateTab
// now takes the same op-lock before spawning (#1195), it blocks until the archive
// releases and then rejects on the resulting Archived status — never interleaving
// a spawn with the move. Modeled deterministically: hold the op-lock + busy set
// (as ArchiveSession does), launch CreateTab, then flip status Archived and
// release (as the archive completing does); CreateTab must return an error having
// spawned nothing.
func TestCreateTab_SerializedWithInFlightArchiveDoesNotOrphan(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	const title, agentName = "worker", "af_worker_agent"
	inst, _, isAlive := registerArchivableWithTmux(t, manager, repoID, repoPath, title, agentName)

	key := daemonInstanceKey(repoID, title)
	// Model an ArchiveSession in flight: busy set registered + op-lock held across
	// the teardown+move window.
	manager.mu.Lock()
	manager.killsInFlight[key] = struct{}{}
	manager.mu.Unlock()
	opLock := manager.opLockFor(key)
	opLock.Lock()

	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		name, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Command: "btop"})
		done <- result{name, err}
	}()

	// Complete the "archive" under the lock — flip to the terminal Archived status
	// exactly as ArchiveSession.SetArchived does — then release so CreateTab can
	// proceed and observe it. Because CreateTab is serialized behind the op-lock,
	// it cannot have spawned anything before this point.
	inst.SetStatus(session.Archived)
	manager.mu.Lock()
	delete(manager.killsInFlight, key)
	manager.mu.Unlock()
	opLock.Unlock()

	res := <-done
	require.Error(t, res.err, "CreateTab racing an in-flight archive must be rejected, not orphan a session")
	assert.Contains(t, res.err.Error(), "archived")
	assert.Equal(t, "", res.name)
	assert.False(t, isAlive(agentName+"__btop"), "no process tmux session may be spawned during/after the archive move")
}

// TestArchiveSession_ModeBTearsDownExtraTabsNoOrphan pins the Phase 2b archive
// mode (teardownArchive) structurally: archiving a session that has a live
// shell/process tab must tear that tab's tmux DOWN and reduce the instance to
// the agent tab alone (#1028) — leaving no orphaned session behind — with the
// worktree relocated in the same folded teardown step (#1195 Ph2b). Together
// with the OpArchiving fence over the teardown+move window (exercised by
// TestCreateTab_SerializedWithInFlightArchiveDoesNotOrphan), this closes the
// archive-orphan gap (audit #2/#5) by construction rather than by convention.
func TestArchiveSession_ModeBTearsDownExtraTabsNoOrphan(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	const title, agentName = "worker", "af_worker_agent"
	inst, srcPath, isAlive := registerArchivableWithTmux(t, manager, repoID, repoPath, title, agentName)

	// Give the session a second (process) tab BEFORE archiving, so the archive
	// teardown has an extra live tmux session it must not orphan into the worktree
	// it is about to move.
	_, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Command: "btop"})
	require.NoError(t, err, "a process tab must spawn on a live session")
	require.Equal(t, 2, inst.TabCount(), "the session now has agent + process tabs")
	require.True(t, isAlive(agentName+"__btop"), "the process tab's tmux session is live before archive")

	_, err = manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)

	// Mode B: the extra tab is torn down and dropped, only the agent name-holder
	// remains, and the worktree has been relocated — no orphan left behind.
	assert.Equal(t, 1, inst.TabCount(), "archive reduces the instance to the agent tab alone (#1028)")
	assert.False(t, isAlive(agentName+"__btop"), "the process tab's tmux must be killed, not orphaned into the moved-away worktree")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(), "a successful archive marks the session Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "the OpArchiving fence is cleared on success")
	assert.False(t, exists(srcPath), "the worktree was moved out of its original path")
}

// TestCreateTab_ShellRejectedAfterArchive is the Shell=true (TUI `t`) counterpart
// of TestCreateTab_RejectedAfterArchive: the shell-tab path is guarded too.
func TestCreateTab_ShellRejectedAfterArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	const title, agentName = "worker", "af_worker_agent"
	_, _, isAlive := registerArchivableWithTmux(t, manager, repoID, repoPath, title, agentName)

	_, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)

	_, err = manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Shell: true})
	require.Error(t, err, "shell CreateTab on an archived session must be rejected")
	assert.Contains(t, err.Error(), "archived")
	assert.False(t, isAlive(agentName+"__shell"), "no shell tmux session may be spawned into the moved-away worktree")
}

package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReserveCreateRefusesBranchHeldByNamedLiveLane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branch := manager.branchForTitle("incoming")
	holderPath := filepath.Join(t.TempDir(), "holder")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, holderPath).CombinedOutput()
	require.NoError(t, err, string(out))

	worktree, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, holderPath, "live-holder", manager.branchForTitle("live-holder"), "", false, true)
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "live-holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(worktree)
	inst.Branch = manager.branchForTitle("live-holder")
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	require.NoError(t, appendInstanceData(repoID, inst.ToInstanceData()))
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "incoming",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "af must refuse before a second live workspace is bound to the held ref")
	assert.Nil(t, renamed)
	msg := err.Error()
	assert.Contains(t, msg, branch)
	assert.Contains(t, msg, "live-holder", "the refusal must name the other lane, not only its filesystem path")
	assert.True(t, strings.Contains(msg, "handoff") || strings.Contains(msg, "archive"),
		"the refusal must tell the operator how to continue safely: %s", msg)
}

func TestSessionStatusProjectsWorktreeIntegrityWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "unsafe-lane", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	manager.worktreeInspector = func([]session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{
			InstanceID: inst.ID,
			Title:      inst.Title,
			Warning:    "DANGER: HEAD moved without a worktree-local reflog entry; do not commit",
		}}
	}
	_, events := manager.events.subscribe()

	manager.refreshWorktreeIntegrityWarnings()
	rows := manager.Snapshot(repoID)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].WorktreeWarning, "DANGER")
	assert.Contains(t, rows[0].WorktreeWarning, "do not commit")
	event := drainNextSessionEvent(t, events, agentproto.EventSessionUpdated)
	assert.Equal(t, rows[0].WorktreeWarning, event.WorktreeWarning,
		"the events client must receive the safety projection without waiting for another status change")
}

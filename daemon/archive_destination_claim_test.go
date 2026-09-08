package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func legacyArchivePair(t *testing.T, titles ...string) (*Manager, string, []*session.Instance) {
	t.Helper()
	m, repoID, repoPath := newStatusTestManager(t)
	var instances []*session.Instance
	var disk []session.InstanceData
	if len(titles) == 0 {
		titles = []string{"feature/login", "feature-login"}
	}
	for _, title := range titles {
		path := filepath.Join(t.TempDir(), "worktree")
		branch := "af/" + title
		out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, path).CombinedOutput()
		require.NoError(t, err, string(out))
		gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, path, title, branch, "", false, true)
		require.NoError(t, err)
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetGitWorktreeForTest(gw)
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Ready)
		inst.AddTabForTest("agent", session.TabKindAgent)
		inst.AddTabForTest("shell", session.TabKindShell)
		inst.AddTabForTest("watcher", session.TabKindProcess)
		m.instances[daemonInstanceKey(repoID, title)] = inst
		instances = append(instances, inst)
		disk = append(disk, inst.ToInstanceData())
	}
	require.NoError(t, config.UpdateRepoInstances(repoID, func(json.RawMessage) (json.RawMessage, error) { return json.Marshal(disk) }))
	return m, repoID, instances
}

func TestArchiveDestinationConcurrentLegacyClaims(t *testing.T) {
	m, repoID, instances := legacyArchivePair(t)
	winner, loser := instances[0], instances[1]
	before := loser.ToInstanceData()
	entered, unblock := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	release := func() { unblockOnce.Do(func() { close(unblock) }) }
	defer release()
	old := archiveTeardown
	loserTeardown := false
	archiveTeardown = func(inst *session.Instance, dest string, claim sessiongit.RelocationClaim, hook func() error, trust bool) (error, error) {
		if inst == loser {
			loserTeardown = true
		}
		if inst == winner {
			close(entered)
			<-unblock
		}
		return old(inst, dest, claim, hook, trust)
	}
	t.Cleanup(func() { archiveTeardown = old })
	// Exercise the archive body independently of the public wrapper's task lock.
	// This fixture has no task writers; the empty task snapshot is immutable.
	archive := func(inst *session.Instance) error {
		_, _, err := m.archiveSession(ArchiveSessionRequest{ID: inst.ID, RepoID: repoID}, map[string][]task.Task{}, nil)
		return err
	}
	first := make(chan error, 1)
	go func() { first <- archive(winner) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first archive did not reach the pre-move gate")
	}
	err := archive(loser)
	assert.Error(t, err, "the second archive must refuse while the first holds its destination")
	if err != nil {
		assert.Contains(t, err.Error(), "is being claimed by session")
		assert.Contains(t, err.Error(), winner.Title)
	}
	assert.False(t, loserTeardown, "the contender must refuse before entering tab teardown")
	assert.Equal(t, before.Tabs, loser.ToInstanceData().Tabs, "refusal must leave every tab untouched")
	assert.Equal(t, before.Liveness, loser.GetLiveness())
	assert.Equal(t, session.OpNone, loser.GetInFlightOp())
	assert.ErrorContains(t, archive(loser), "is being claimed by session", "a refused contender must not release the winner's claim")
	release()
	require.NoError(t, <-first)
	require.Equal(t, session.Archived, winner.GetStatus())
	require.Empty(t, m.reservedArchiveDestinations, "success must release the claim")
}

func TestArchiveDestinationRetryThroughHomeAlias(t *testing.T) {
	m, _, _ := newStatusTestManager(t)
	home := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(home, alias))
	dest := filepath.Join(home, "archived", "repo", "worker")
	require.NoError(t, os.MkdirAll(dest, 0755))
	source := filepath.Join(alias, "archived", "repo", "worker")
	inst := &session.Instance{Title: "worker"}
	moveDest, err := m.checkArchiveDestination("repo", inst, dest, source)
	require.NoError(t, err)
	defer m.releaseArchiveDestination("repo", inst, dest)
	require.Equal(t, source, moveDest, "the move must take the already-at-destination repair path")
}

func TestArchiveDestinationClaimReleasedAfterMoveFailure(t *testing.T) {
	m, repoID, instances := legacyArchivePair(t)
	old := archiveTeardown
	t.Cleanup(func() { archiveTeardown = old })
	archiveTeardown = func(*session.Instance, string, sessiongit.RelocationClaim, func() error, bool) (error, error) {
		return nil, errors.New("forced move failure")
	}
	_, _, err := m.ArchiveSession(ArchiveSessionRequest{ID: instances[0].ID, RepoID: repoID})
	archiveTeardown = old
	require.ErrorContains(t, err, "forced move failure")
	_, _, err = m.ArchiveSession(ArchiveSessionRequest{ID: instances[1].ID, RepoID: repoID})
	require.NoError(t, err, "a failed archive must release its destination claim")
}

func TestArchiveDestinationAliasRetryCompletes(t *testing.T) {
	m, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, m, repoID, repoPath, "worker")
	dest, err := archivedWorktreePath(repoID, inst.Title)
	require.NoError(t, err)
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	// Model a move that completed before the daemon committed Archived.
	require.NoError(t, gw.ArchiveWorktree(dest))
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "home-alias")
	require.NoError(t, os.Symlink(home, alias))
	source := filepath.Join(alias, "archived", repoID, inst.Title)
	stored := inst.ToInstanceData().Worktree
	restored, err := sessiongit.NewGitWorktreeFromStorage(repoPath, source, inst.Title, stored.BranchName, stored.BaseCommitSHA, false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(restored)
	path, _, err := m.ArchiveSession(ArchiveSessionRequest{ID: inst.ID, RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, source, path)
	require.Equal(t, session.Archived, inst.GetStatus())
	require.Empty(t, m.reservedArchiveDestinations)
}

func TestArchiveDestinationReservationsReleaseOnlyTheirOwner(t *testing.T) {
	m, _, _ := newStatusTestManager(t)
	owner := &session.Instance{Title: "Feature/login"}
	other := &session.Instance{Title: "feature-login"}
	dest := filepath.Join(t.TempDir(), "Feature-login")
	require.NoError(t, os.Mkdir(dest, 0755))
	_, err := m.checkArchiveDestination("repo", owner, dest, "missing")
	require.Error(t, err)
	require.Empty(t, m.reservedArchiveDestinations, "a failed occupancy probe must release its claim")
	require.NoError(t, os.Remove(dest))
	_, err = m.checkArchiveDestination("repo", owner, dest, "missing")
	require.NoError(t, err)
	defer m.releaseArchiveDestination("repo", owner, dest)
	// Neither an unrelated release nor a portable spelling variant can steal it.
	m.releaseArchiveDestination("repo", other, dest)
	_, err = m.checkArchiveDestination("repo", other, filepath.Join(filepath.Dir(dest), "feature-login"), "missing")
	require.ErrorContains(t, err, `is being claimed by session "Feature/login"`)
	m.releaseArchiveDestination("repo", owner, dest)
	_, err = m.checkArchiveDestination("repo", other, dest, "missing")
	require.NoError(t, err)
	// An old owner's deferred release must not erase the new owner's claim.
	m.releaseArchiveDestination("repo", owner, dest)
	require.Len(t, m.reservedArchiveDestinations, 1)
	m.releaseArchiveDestination("repo", other, dest)
	require.Empty(t, m.reservedArchiveDestinations)
}

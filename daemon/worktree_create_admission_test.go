package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inPlaceAdmissionFakeBackend struct {
	readyFakeBackend
}

func (b inPlaceAdmissionFakeBackend) Start(instance *session.Instance, firstTimeSetup bool) error {
	if err := b.Provision(instance, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(instance, firstTimeSetup)
}

func (b inPlaceAdmissionFakeBackend) Provision(instance *session.Instance, firstTimeSetup bool) error {
	if !firstTimeSetup {
		return nil
	}
	worktree, branch, err := sessiongit.NewGitWorktreeInPlace(instance.Path)
	if err != nil {
		return err
	}
	instance.SetGitWorktreeForTest(worktree)
	instance.Branch = branch
	return nil
}

func TestReserveCreateRefusesBranchHeldByNamedLiveLane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branch := manager.branchForTitle("incoming")
	holderPath := filepath.Join(t.TempDir(), "holder")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, holderPath).CombinedOutput()
	require.NoError(t, err, string(out))

	live := registerCreateAdmissionLane(t, manager, repoID, repoPath, holderPath, "-live-holder", branch)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "incoming",
		Program:  "claude",
	})
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Nil(t, renamed)
	assert.Contains(t, err.Error(), branch)
	assert.Contains(t, err.Error(), live.Title)
	assert.Contains(t, err.Error(), "af sessions handoff --to '<agent>' -- -live-holder")
}

func TestReserveCreateInPlaceUsesActualBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	actualBranch := "shared-in-place"
	livePath := filepath.Join(t.TempDir(), "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-q", "-b", actualBranch, livePath).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "checkout", "-q", "--ignore-other-worktrees", "-B", actualBranch, actualBranch).CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCreateAdmissionLane(t, manager, repoID, repoPath, livePath, "live-holder", actualBranch)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "title-derived-branch-is-different",
		Program:  "claude",
		InPlace:  true,
	})
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Nil(t, renamed)
	assert.Contains(t, err.Error(), actualBranch)
	assert.Contains(t, err.Error(), live.Title)
	assert.NotContains(t, err.Error(), manager.branchForTitle("title-derived-branch-is-different"))
}

func TestReserveCreateInPlaceRefusesDetachedLiveWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	out, err := exec.Command("git", "-C", repoPath, "checkout", "-q", "--detach").CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCreateAdmissionLane(t, manager, repoID, repoPath, repoPath, "live-detached", "HEAD")
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "incoming-detached",
		Program:  "claude",
		InPlace:  true,
	})
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Nil(t, renamed)
	assert.Contains(t, err.Error(), live.Title)
	assert.Contains(t, err.Error(), "detached HEAD")
}

func TestReserveCreateInPlaceFindsLiveHolderUnderHistoricalRepoKey(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branchOut, err := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "HEAD").CombinedOutput()
	require.NoError(t, err, string(branchOut))
	live := registerCreateAdmissionLane(t, manager, repoID, repoPath, repoPath, "historical-holder", strings.TrimSpace(string(branchOut)))
	historicalRepoID := config.RepoIDFromRoot(filepath.Join(t.TempDir(), "pre-rename-repo"))
	require.NotEqual(t, repoID, historicalRepoID)
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, live.Title))
	manager.instances[daemonInstanceKey(historicalRepoID, live.Title)] = live
	manager.mu.Unlock()

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "incoming-here",
		Program:  "claude",
		InPlace:  true,
	})
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Nil(t, renamed)
	assert.Contains(t, err.Error(), live.Title)
}

func TestConcurrentInPlaceCreatesReserveBranchAdmission(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	parent, _, firstPath := setupBareCloneWorktree3358(t)
	secondPath := filepath.Join(parent, "second-worktree")
	out, err := exec.Command("git", "-C", firstPath, "worktree", "add", "-q", "-b", "second-staging", secondPath, "HEAD").CombinedOutput()
	require.NoError(t, err, string(out))
	branchOut, err := exec.Command("git", "-C", firstPath, "symbolic-ref", "--short", "HEAD").CombinedOutput()
	require.NoError(t, err, string(branchOut))
	branch := strings.TrimSpace(string(branchOut))
	out, err = exec.Command("git", "-C", secondPath, "checkout", "-q", "--ignore-other-worktrees", "-B", branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))
	firstRepo, err := config.RepoFromPath(firstPath)
	require.NoError(t, err)
	secondRepo, err := config.RepoFromPath(secondPath)
	require.NoError(t, err)
	require.Equal(t, firstRepo.ID, secondRepo.ID)

	backend := session.NewFakeBackend()
	backend.CompleteStart()
	entered, unblock := blockingCreateFactory(t, inPlaceAdmissionFakeBackend{readyFakeBackend{backend}}, nil)
	server := &controlServer{manager: manager}
	firstDone := startCreateCall(server, CreateSessionRequest{Title: "first-here", RepoPath: firstPath, Program: "claude", InPlace: true})
	waitForCreateFactory(t, entered)
	secondDone := startCreateCall(server, CreateSessionRequest{Title: "second-here", RepoPath: secondPath, Program: "claude", InPlace: true})
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.pendingCreates) == 1
	}, time.Second, 5*time.Millisecond)
	select {
	case result := <-secondDone:
		t.Fatalf("second create did not wait for branch admission: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}

	unblock()
	first := waitForCreateResult(t, firstDone)
	second := waitForCreateResult(t, secondDone)
	require.NoError(t, first.err)
	require.Error(t, second.err)
	assert.Contains(t, second.err.Error(), branch)
	assert.Contains(t, second.err.Error(), "first-here")
	assert.NotContains(t, second.err.Error(), "already exists")
	assert.NotContains(t, second.err.Error(), "already reserved")
}

func TestConcurrentSameTitleCreateKeepsDuplicateTitleError(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	backend := session.NewFakeBackend()
	backend.CompleteStart()
	entered, unblock := blockingCreateFactory(t, inPlaceAdmissionFakeBackend{readyFakeBackend{backend}}, nil)
	server := &controlServer{manager: manager}
	request := CreateSessionRequest{Title: "dupe", RepoPath: repoPath, Program: "claude", InPlace: true}
	firstDone := startCreateCall(server, request)
	waitForCreateFactory(t, entered)
	secondDone := startCreateCall(server, request)
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.pendingCreates) == 1
	}, time.Second, 5*time.Millisecond)
	select {
	case result := <-secondDone:
		t.Fatalf("duplicate create did not wait for title admission: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}

	unblock()
	first := waitForCreateResult(t, firstDone)
	second := waitForCreateResult(t, secondDone)
	require.NoError(t, first.err)
	require.Error(t, second.err)
	assert.True(t,
		strings.Contains(second.err.Error(), "already exists") || strings.Contains(second.err.Error(), "reserved"),
		"duplicate create error should preserve the title-reservation contract: %v", second.err,
	)
	assert.NotContains(t, second.err.Error(), "already checked out by live lane")
}

func TestReserveCreateKeepsEveryHolderBeforeArchivedRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branch := manager.branchForTitle("incoming")
	recordedRoot := filepath.Join(t.TempDir(), "recorded-root")
	require.NoError(t, os.Symlink(filepath.Dir(repoPath), recordedRoot))
	livePath := filepath.Join(recordedRoot, "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", "live-staging", livePath).CombinedOutput()
	require.NoError(t, err, string(out))
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "incoming", "incoming")
	out, err = exec.Command("git", "-C", livePath, "checkout", "--ignore-other-worktrees", "-q", "-B", branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCreateAdmissionLane(t, manager, repoID, repoPath, livePath, "live-holder", branch)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	holds, err := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, err)
	require.Len(t, holds[branch], 2)
	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{RepoPath: repoPath, Title: "incoming", Program: "claude"})
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Nil(t, renamed)
	assert.Equal(t, "incoming", archived.Title)
	assert.Contains(t, err.Error(), live.Title)
}

func registerCreateAdmissionLane(t *testing.T, manager *Manager, repoID, repoPath, worktreePath, title, branch string) *session.Instance {
	t.Helper()
	worktree, err := sessiongit.NewGitWorktreeFromStorage(repoPath, worktreePath, title, branch, "", false, true)
	require.NoError(t, err)
	instance, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	instance.SetBackend(session.NewFakeBackend())
	instance.SetGitWorktreeForTest(worktree)
	instance.Branch = branch
	instance.SetStartedForTest(true)
	instance.SetStatusForTest(session.Ready)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, title)] = instance
	manager.mu.Unlock()
	return instance
}

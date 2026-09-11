package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inPlaceWorktreeFakeBackend struct {
	readyFakeBackend
}

func (b inPlaceWorktreeFakeBackend) Start(instance *session.Instance, firstTimeSetup bool) error {
	if err := b.Provision(instance, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(instance, firstTimeSetup)
}

func (b inPlaceWorktreeFakeBackend) Provision(instance *session.Instance, firstTimeSetup bool) error {
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

func TestUnreadableWorktreeScanCannotReportClean(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "unknown", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Title: inst.Title, Err: assert.AnError}}
	}

	manager.refreshWorktreeIntegrityWarnings()
	assert.Contains(t, inst.WorktreeWarning(), "could not be verified")
}

func TestMissingWorktreeInspectionCannotClearConfirmedWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Warning: "DANGER: confirmed duplicate branch"}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection { return nil }
	manager.refreshWorktreeIntegrityWarnings()
	assert.Contains(t, inst.WorktreeWarning(), "confirmed duplicate branch")
	assert.Contains(t, inst.WorktreeWarning(), "could not be verified")
}

func TestSkippedPersistedRepoCannotClearConfirmedWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	confirmed := "DANGER: confirmed duplicate branch"
	require.True(t, inst.ReconcileWorktreeInspection(confirmed, nil))
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID}}
	}
	manager.mu.Lock()
	manager.setWorktreeInventoryLocked(worktreeInventoryState{incompleteRepo: map[string]error{
		repoID: errors.New("persisted peer was skipped"),
	}})
	manager.mu.Unlock()

	manager.refreshWorktreeIntegrityWarnings()

	assert.Contains(t, inst.WorktreeWarning(), confirmed)
	assert.Contains(t, inst.WorktreeWarning(), "persisted peer was skipped",
		"a skipped required observation is unknown and cannot prove a prior collision was repaired")
}

func TestPersistedInventoryGapClearsWarningForArchivedLane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "archived", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.ReconcileWorktreeInspection("DANGER: stale live-worktree warning", nil))
	inst.SetStatusForTest(session.Archived)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.setWorktreeInventoryLocked(worktreeInventoryState{incompleteRepo: map[string]error{
		repoID: errors.New("persisted peer was skipped"),
	}})
	manager.mu.Unlock()
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return nil
	}

	manager.refreshWorktreeIntegrityWarnings()

	assert.Empty(t, inst.WorktreeWarning(),
		"an archived lane is positively outside live-worktree inspection, so another row's inventory gap must not keep its warning")
}

func TestPartialWorktreeScanCannotClearConfirmedWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Warning: "DANGER: confirmed duplicate branch"}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, CorrelationErr: assert.AnError}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	assert.Contains(t, inst.WorktreeWarning(), "confirmed duplicate branch")
	assert.Contains(t, inst.WorktreeWarning(), "could not be verified")
}

func TestCompleteWorktreeScanCanClearConfirmedWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	warning := "DANGER: confirmed duplicate branch"
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Warning: warning}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	assert.Empty(t, inst.WorktreeWarning(), "only a complete clean scan may clear the last confirmed warning")
}

func TestStaleWorktreeScanCannotClearRestoredLaneWarning(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	confirmed := "DANGER: confirmed duplicate branch"
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Warning: confirmed}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	inst.SetStatusForTest(session.Archived)

	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan []session.InstanceData, 1)
	manager.worktreeInspector = func(_ context.Context, rows []session.InstanceData) []session.SessionWorktreeInspection {
		observed <- rows
		close(entered)
		<-release
		return nil
	}
	done := make(chan struct{})
	go func() {
		manager.refreshWorktreeIntegrityWarnings()
		close(done)
	}()
	<-entered
	rows := <-observed
	require.Len(t, rows, 1)
	require.True(t, session.IsArchivedData(rows[0]), "the stale scan must snapshot the lane while archived")
	inst.SetStatusForTest(session.Ready)
	close(release)
	<-done

	assert.Contains(t, inst.WorktreeWarning(), confirmed,
		"an archived snapshot must not clear a warning after the lane becomes live again")
}

func TestWorktreeReconcileCannotClearLaneRestoredAfterValidation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "holder", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
	manager.mu.Unlock()

	confirmed := "DANGER: confirmed duplicate branch"
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: inst.ID, Warning: confirmed}}
	}
	manager.refreshWorktreeIntegrityWarnings()
	inst.SetStatusForTest(session.Archived)
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection { return nil }
	manager.worktreeBeforeReconcile = func() {
		inst.SetStatusForTest(session.Ready)
	}

	manager.refreshWorktreeIntegrityWarnings()

	assert.Contains(t, inst.WorktreeWarning(), confirmed,
		"identity validation and warning reconciliation must be one atomic instance operation")
}

func TestWorktreeReconcileCannotClearWhenCorrelatedPeerRestoresAfterValidation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	newLane := func(title string) *session.Instance {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
		require.NoError(t, err)
		inst.SetBackend(session.NewFakeBackend())
		inst.SetStartedForTest(true)
		manager.mu.Lock()
		manager.instances[daemonInstanceKey(repoID, inst.Title)] = inst
		manager.mu.Unlock()
		return inst
	}
	holder := newLane("holder")
	peer := newLane("peer")
	confirmed := "DANGER: confirmed duplicate branch"
	require.True(t, holder.ReconcileWorktreeInspection(confirmed, nil))
	peer.SetStatusForTest(session.Archived)

	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		return []session.SessionWorktreeInspection{{InstanceID: holder.ID}}
	}
	manager.worktreeBeforeReconcile = func() {
		peer.SetStatusForTest(session.Ready)
	}

	manager.refreshWorktreeIntegrityWarnings()

	assert.Contains(t, holder.WorktreeWarning(), confirmed,
		"a clean correlated result must not apply after any peer changes applicability")
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

	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
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

func TestStalledWorktreeScanDoesNotBlockPollRecovery(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &deadButRecoverableBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "stranded", backend, true, session.Running)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	manager.worktreeInspector = func(context.Context, []session.InstanceData) []session.SessionWorktreeInspection {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}

	stopCh := make(chan struct{})
	var loops sync.WaitGroup
	loops.Add(1)
	go func() {
		defer loops.Done()
		manager.refreshWorktreeIntegrityWarnings()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("integrity scan never entered the stalled inspector")
	}
	startInstancePollLoop(manager, 50*time.Millisecond, stopCh, &loops)
	t.Cleanup(func() {
		close(release)
		close(stopCh)
		loops.Wait()
		manager.waitRootAgentCreates()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, recovers := backend.counts()
		if recovers > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll loop did not reach Lost-session recovery while the diagnostic worktree scan was outstanding")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWorktreeIntegrityLoopShutdownDoesNotWaitForScan(t *testing.T) {
	manager, _, _ := newStatusTestManager(t)
	entered := make(chan struct{})
	manager.worktreeInspector = func(ctx context.Context, _ []session.InstanceData) []session.SessionWorktreeInspection {
		close(entered)
		<-ctx.Done()
		return nil
	}

	stopCh := make(chan struct{})
	var loops sync.WaitGroup
	startWorktreeIntegrityLoop(manager, time.Hour, stopCh, &loops)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("integrity scan never started")
	}
	close(stopCh)
	done := make(chan struct{})
	go func() {
		loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("daemon shutdown waited for the outstanding integrity scan")
	}
}

func TestArchivedRestoreAdmissionUsesWorktreeActualBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "actual-branch")
	actualBranch := "manually-selected"
	out, err := exec.Command("git", "-C", archived.GetWorktreePath(), "checkout", "-q", "-b", actualBranch).CombinedOutput()
	require.NoError(t, err, string(out))

	livePath := filepath.Join(t.TempDir(), "live")
	out, err = exec.Command("git", "-C", repoPath, "worktree", "add", "-q", "-b", "live-staging", livePath).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", livePath, "checkout", "-q", "--ignore-other-worktrees", "-B", actualBranch, actualBranch).CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCollisionLane(t, manager, repoID, repoPath, livePath, "live-holder", actualBranch, session.Ready)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	release, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true, archived.GetWorktreePath())
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), actualBranch)
	assert.Contains(t, err.Error(), live.Title)
	assert.NotContains(t, err.Error(), manager.branchForTitle(archived.Title),
		"the cached archived branch must not decide admission after a manual checkout")
}

func TestArchivedRestoreRefusesLiveLaneSharingRetainedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "shared-target")
	live := registerCollisionLane(t, manager, repoID, repoPath, archived.GetWorktreePath(), "live-here", archived.GetBranch(), session.Ready)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	release, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true, archived.GetWorktreePath())
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), live.Title)
	assert.Contains(t, err.Error(), archived.GetWorktreePath())
}

func TestArchivedRestoreFindsLiveHolderUnderHistoricalRepoKey(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "historical-restore")
	historicalRepoID := config.RepoIDFromRoot(filepath.Join(t.TempDir(), "pre-rename-repo"))
	require.NotEqual(t, repoID, historicalRepoID)
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, archived.Title))
	manager.instances[daemonInstanceKey(historicalRepoID, archived.Title)] = archived
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(historicalRepoID, archived.ToInstanceData()))

	livePath := filepath.Join(t.TempDir(), "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-q", "-b", "live-staging", livePath).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", livePath, "checkout", "-q", "--ignore-other-worktrees", "-B", archived.GetBranch(), archived.GetBranch()).CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCollisionLane(t, manager, repoID, repoPath, livePath, "current-key-holder", archived.GetBranch(), session.Ready)

	release, err := manager.reserveLocalRestoreBranch(historicalRepoID, archived.Title, archived, true, archived.GetWorktreePath())
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), live.Title,
		"Git's holder path must be matched against the complete live roster after a repository-key rename")
}

func TestArchivedRestoreRejectsWorktreeFromDifferentRepository(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "foreign-worktree")
	foreignRepo := setupControlRepo(t)

	release, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true, foreignRepo)
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to repository",
		"a branch observation from an unrelated repository must not satisfy the restore identity tuple")
}

func TestArchivedRestoreIgnoresStaleRelocationAliasForBranchAdmission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "stale-alias")
	selectedPath := archived.GetWorktreePath()
	selectedBranch := archived.GetBranch()
	info, err := os.Stat(selectedPath)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	staleAlias := t.TempDir()
	worktree, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, selectedPath, archived.Title, selectedBranch, "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryMoveUnknown,
		AlternatePath: staleAlias,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	archived.SetGitWorktreeForTest(worktree)

	peerPath := t.TempDir()
	peer := registerCollisionLane(t, manager, repoID, repoPath, peerPath, "live-branch-peer", selectedBranch, session.Ready)
	require.NoError(t, appendInstanceData(repoID, peer.ToInstanceData()))

	originalBindings := worktreeBranchBindings
	worktreeBranchBindings = func(string) ([]sessiongit.WorktreeBranchBinding, error) {
		return []sessiongit.WorktreeBranchBinding{
			{Path: staleAlias, Branch: "replacement-branch", HeadSHA: strings.Repeat("1", 40)},
			{Path: selectedPath, Branch: selectedBranch, HeadSHA: strings.Repeat("2", 40)},
			{Path: peerPath, Branch: selectedBranch, HeadSHA: strings.Repeat("2", 40)},
		}, nil
	}
	t.Cleanup(func() { worktreeBranchBindings = originalBindings })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: archived.Title, RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), peer.Title,
		"a stale, replaced alternate must not select the branch used to admit the identity-qualified restore source")
}

func TestDetachedArchivedRestoreRefusesLiveLaneSharingRetainedWorktree(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "archived", "detached-shared-target")
	out, err := exec.Command("git", "-C", archived.GetWorktreePath(), "checkout", "-q", "--detach").CombinedOutput()
	require.NoError(t, err, string(out))
	live := registerCollisionLane(t, manager, repoID, repoPath, archived.GetWorktreePath(), "live-here", "HEAD", session.Ready)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))

	release, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true, archived.GetWorktreePath())
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), live.Title)
	assert.Contains(t, err.Error(), "detached HEAD")
}

func TestDetachedArchivedRestoreHoldsWorktreeAdmission(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	first, _ := seedArchivedSession(t, manager, repoID, repoPath, "first", "detached-first")
	second, _ := seedArchivedSession(t, manager, repoID, repoPath, "second", "detached-second")
	for _, instance := range []*session.Instance{first, second} {
		out, err := exec.Command("git", "-C", instance.GetWorktreePath(), "checkout", "-q", "--detach").CombinedOutput()
		require.NoError(t, err, string(out))
	}

	releaseFirst, err := manager.reserveLocalRestoreBranch(repoID, first.Title, first, true, first.GetWorktreePath())
	require.NoError(t, err)
	released := false
	t.Cleanup(func() {
		if !released {
			releaseFirst()
		}
	})
	done := make(chan error, 1)
	go func() {
		release, reserveErr := manager.reserveLocalRestoreBranch(repoID, second.Title, second, true, second.GetWorktreePath())
		if release != nil {
			release()
		}
		done <- reserveErr
	}()
	select {
	case err := <-done:
		t.Fatalf("detached restore did not hold shared worktree admission: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releaseFirst()
	released = true
	require.NoError(t, <-done)
}

func TestLostRecoveryRefusesLiveBranchPeer(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			subject, _ := seedArchivedSession(t, manager, repoID, repoPath, "lost", "lost-collision")
			branch := manager.branchForTitle(subject.Title)
			peerPath := filepath.Join(t.TempDir(), "peer")
			out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-q", "-b", "peer-staging", peerPath).CombinedOutput()
			require.NoError(t, err, string(out))
			out, err = exec.Command("git", "-C", peerPath, "checkout", "-q", "--ignore-other-worktrees", "-B", branch, branch).CombinedOutput()
			require.NoError(t, err, string(out))
			registerCollisionLane(t, manager, repoID, repoPath, peerPath, "peer", branch, session.Ready)

			backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
			subject.SetBackend(backend)
			subject.SetStartedForTest(true)
			subject.SetStatusForTest(session.Lost)
			if automatic {
				manager.restoreLostSession(daemonInstanceKey(repoID, subject.Title), repoID, subject)
			} else {
				_, err = manager.restoreLostOrDeadSession(repoID, subject.Title, subject, false)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "peer")
			}
			assert.Zero(t, backend.recoverCalls(), "branch admission must refuse before Recover starts")
		})
	}
}

func TestArchivedRestoreAdmissionSerializesMultiplyBoundPeers(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	first, _ := seedArchivedSession(t, manager, repoID, repoPath, "first", "restore-first")
	second, _ := seedArchivedSession(t, manager, repoID, repoPath, "second", "restore-second")
	branch := manager.branchForTitle(first.Title)
	out, err := exec.Command("git", "-C", second.GetWorktreePath(), "checkout", "-q", "--ignore-other-worktrees", "-B", branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))

	releaseFirst, err := manager.reserveLocalRestoreBranch(repoID, first.Title, first, true, first.GetWorktreePath())
	require.NoError(t, err)
	releasedFirst := false
	t.Cleanup(func() {
		if !releasedFirst {
			releaseFirst()
		}
	})

	type result struct {
		release func()
		err     error
	}
	done := make(chan result, 1)
	go func() {
		release, reserveErr := manager.reserveLocalRestoreBranch(repoID, second.Title, second, true, second.GetWorktreePath())
		done <- result{release: release, err: reserveErr}
	}()
	select {
	case got := <-done:
		if got.release != nil {
			got.release()
		}
		t.Fatal("second restore admission did not wait for the shared branch reservation")
	case <-time.After(100 * time.Millisecond):
	}
	first.SetStatusForTest(session.Ready)
	releaseFirst()
	releasedFirst = true
	got := <-done
	if got.release != nil {
		got.release()
	}
	require.Error(t, got.err)
	assert.Contains(t, got.err.Error(), first.Title,
		"the waiter must revalidate after the reservation and see the first lane become live")
}

func TestArchivedRestoreAdmissionSerializesWithInPlaceCreate(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "restoring", "restoring")
	branch := archived.GetBranch()
	out, err := exec.Command("git", "-C", repoPath, "checkout", "-q", "--ignore-other-worktrees", "-B", branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))

	releaseRestore, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true, archived.GetWorktreePath())
	require.NoError(t, err)
	releasedRestore := false
	t.Cleanup(func() {
		if !releasedRestore {
			releaseRestore()
		}
	})
	backend := session.NewFakeBackend()
	backend.CompleteStart()
	restoreFactory := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return inPlaceWorktreeFakeBackend{readyFakeBackend{backend}}, nil
	})
	t.Cleanup(restoreFactory)
	done := startCreateCall(&controlServer{manager: manager}, CreateSessionRequest{
		Title: "incoming-here", RepoPath: repoPath, Program: "claude", InPlace: true,
	})

	select {
	case result := <-done:
		t.Fatalf("in-place create passed while an archived restore held branch admission: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	archived.SetStatusForTest(session.Ready)
	require.NoError(t, persistInstanceData(repoID, archived.ToInstanceData()))
	releaseRestore()
	releasedRestore = true
	result := waitForCreateResult(t, done)
	require.Error(t, result.err)
	assert.Contains(t, result.err.Error(), archived.Title,
		"the create must revalidate after the shared admission reservation and name the restored holder")
}

func TestArchivedRestoreAdmissionUsesCanonicalRepoIdentity(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "restoring", "historical-lock")
	historicalRepoID := config.RepoIDFromRoot(filepath.Join(t.TempDir(), "pre-rename-repo"))
	require.NotEqual(t, repoID, historicalRepoID)
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, archived.Title))
	manager.instances[daemonInstanceKey(historicalRepoID, archived.Title)] = archived
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(historicalRepoID, archived.ToInstanceData()))

	out, err := exec.Command("git", "-C", repoPath, "checkout", "-q", "--ignore-other-worktrees", "-B", archived.GetBranch(), archived.GetBranch()).CombinedOutput()
	require.NoError(t, err, string(out))
	releaseRestore, err := manager.reserveLocalRestoreBranch(historicalRepoID, archived.Title, archived, true, archived.GetWorktreePath())
	require.NoError(t, err)
	releasedRestore := false
	t.Cleanup(func() {
		if !releasedRestore {
			releaseRestore()
		}
	})

	backend := session.NewFakeBackend()
	backend.CompleteStart()
	restoreFactory := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return inPlaceWorktreeFakeBackend{readyFakeBackend{backend}}, nil
	})
	t.Cleanup(restoreFactory)
	done := startCreateCall(&controlServer{manager: manager}, CreateSessionRequest{
		Title: "incoming-here", RepoPath: repoPath, Program: "claude", InPlace: true,
	})

	select {
	case result := <-done:
		t.Fatalf("current-key create did not share admission with historical-key restore: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	archived.SetStatusForTest(session.Ready)
	require.NoError(t, persistInstanceData(historicalRepoID, archived.ToInstanceData()))
	releaseRestore()
	releasedRestore = true
	result := waitForCreateResult(t, done)
	require.Error(t, result.err)
	assert.Contains(t, result.err.Error(), archived.Title,
		"the waiter must revalidate after the canonical repository reservation")
}

func TestInPlaceArchivedTitleReuseHasNoPostRenameAdmissionFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "reuse", "reuse")
	originalBindings := worktreeBranchBindings
	bindingsCalls := 0
	worktreeBranchBindings = func(path string) ([]sessiongit.WorktreeBranchBinding, error) {
		bindingsCalls++
		if bindingsCalls > 1 {
			return nil, errors.New("later binding probe failed")
		}
		return originalBindings(path)
	}
	t.Cleanup(func() { worktreeBranchBindings = originalBindings })
	backend := session.NewFakeBackend()
	backend.CompleteStart()
	restoreFactory := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return inPlaceWorktreeFakeBackend{readyFakeBackend{backend}}, nil
	})
	t.Cleanup(restoreFactory)

	result := waitForCreateResult(t, startCreateCall(&controlServer{manager: manager}, CreateSessionRequest{
		Title: "reuse", RepoPath: repoPath, Program: "claude", InPlace: true,
	}))
	require.NoError(t, result.err,
		"branch admission must be complete before the archived rename, with no fallible recheck after mutation")
	assert.Equal(t, "reuse", result.resp.Instance.Title)
	assert.Equal(t, "reuse (archived)", archived.Title)
	assert.Equal(t, 1, bindingsCalls)
}

func registerCollisionLane(t *testing.T, manager *Manager, repoID, repoPath, worktreePath, title, branch string, status session.Status) *session.Instance {
	t.Helper()
	worktree, err := sessiongit.NewGitWorktreeFromStorage(repoPath, worktreePath, title, branch, "", false, true)
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetGitWorktreeForTest(worktree)
	inst.Branch = branch
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, title)] = inst
	manager.mu.Unlock()
	return inst
}

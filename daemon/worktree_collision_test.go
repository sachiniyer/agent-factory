package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestReserveCreateRefusesBranchHeldByNamedLiveLane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branch := manager.branchForTitle("incoming")
	liveHolder := "-live-holder"
	holderPath := filepath.Join(t.TempDir(), "holder")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, holderPath).CombinedOutput()
	require.NoError(t, err, string(out))

	worktree, err := sessiongit.NewGitWorktreeFromStorage(
		repoPath, holderPath, liveHolder, manager.branchForTitle(liveHolder), "", false, true)
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{Title: liveHolder, Path: repoPath, Program: "claude"})
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
	assert.Contains(t, msg, liveHolder, "the refusal must name the other lane, not only its filesystem path")
	assert.Contains(t, msg, "af sessions handoff --to '<agent>' -- -live-holder",
		"the actionable command must terminate options before a dash-leading lane title")
	assert.True(t, strings.Contains(msg, "handoff") || strings.Contains(msg, "archive"),
		"the refusal must tell the operator how to continue safely: %s", msg)
}

func TestReserveCreateInPlaceRefusesActualBranchHeldByLiveLane(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	actualBranch := "shared-in-place"
	livePath := filepath.Join(t.TempDir(), "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-q", "-b", actualBranch, livePath).CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "checkout", "-q", "--ignore-other-worktrees", "-B", actualBranch, actualBranch).CombinedOutput()
	require.NoError(t, err, string(out))

	live := registerCollisionLane(t, manager, repoID, repoPath, livePath, "live-holder", actualBranch, session.Ready)
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

	require.Error(t, err, "--here must refuse before activating a second live lane on the target worktree's actual branch")
	assert.Nil(t, renamed)
	assert.Contains(t, err.Error(), actualBranch)
	assert.Contains(t, err.Error(), live.Title, "the refusal must name the live lane holding the actual branch")
	assert.NotContains(t, err.Error(), manager.branchForTitle("title-derived-branch-is-different"),
		"an in-place admission must not derive its branch from the requested title")
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
	require.Equal(t, firstRepo.ID, secondRepo.ID, "both worktrees must share the serialized repository identity")

	backend := session.NewFakeBackend()
	backend.CompleteStart()
	entered, unblock := blockingCreateFactory(t, inPlaceWorktreeFakeBackend{readyFakeBackend{backend}}, nil)
	server := &controlServer{manager: manager}
	firstDone := startCreateCall(server, CreateSessionRequest{
		Title: "first-here", RepoPath: firstPath, Program: "claude", InPlace: true,
	})
	waitForCreateFactory(t, entered)
	secondDone := startCreateCall(server, CreateSessionRequest{
		Title: "second-here", RepoPath: secondPath, Program: "claude", InPlace: true,
	})
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return len(manager.pendingCreates) == 2
	}, time.Second, 5*time.Millisecond,
		"the second create must pass the pre-lock check and wait behind the first under the shared repo admission lock")

	unblock()
	first := waitForCreateResult(t, firstDone)
	second := waitForCreateResult(t, secondDone)
	require.NoError(t, first.err)
	require.Error(t, second.err,
		"the serialized admission point must refuse the second create after the first lane becomes live; roster: %+v",
		manager.Snapshot(firstRepo.ID))
	assert.Contains(t, second.err.Error(), branch)
	assert.Contains(t, second.err.Error(), "first-here")
}

func TestReserveCreateRefusesLiveHolderWhenArchivedHolderIsListedLast(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	branch := manager.branchForTitle("incoming")
	liveTitle := "live-holder"
	// Spell the live worktree through a symlinked root. Git records and reports
	// the resolved path (the same /var -> /private/var class macOS exercises),
	// while the AF row deliberately retains the path the caller supplied.
	recordedRoot := filepath.Join(t.TempDir(), "recorded-root")
	require.NoError(t, os.Symlink(filepath.Dir(repoPath), recordedRoot))
	livePath := filepath.Join(recordedRoot, "live")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", "live-staging", livePath).CombinedOutput()
	require.NoError(t, err, string(out))

	archived, _ := seedArchivedSession(t, manager, repoID, repoPath, "incoming", "incoming")
	out, err = exec.Command("git", "-C", livePath, "checkout", "--ignore-other-worktrees", "-q", "-B", branch, branch).CombinedOutput()
	require.NoError(t, err, string(out))

	worktree, err := sessiongit.NewGitWorktreeFromStorage(repoPath, livePath, liveTitle, branch, "", false, true)
	require.NoError(t, err)
	live, err := session.NewInstance(session.InstanceOptions{Title: liveTitle, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	live.SetBackend(session.NewFakeBackend())
	live.SetGitWorktreeForTest(worktree)
	live.Branch = branch
	live.SetStartedForTest(true)
	live.SetStatusForTest(session.Ready)
	require.NoError(t, appendInstanceData(repoID, live.ToInstanceData()))
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, live.Title)] = live
	manager.mu.Unlock()

	holds, err := sessiongit.BranchesHeldByWorktrees(repoPath)
	require.NoError(t, err)
	resolvedHolders := make([]string, 0, len(holds[branch]))
	for _, holder := range holds[branch] {
		resolvedHolders = append(resolvedHolders, resolvedPath(t, holder))
	}
	assert.ElementsMatch(t, []string{resolvedPath(t, livePath), resolvedPath(t, archived.GetWorktreePath())}, resolvedHolders,
		"precondition: both holders must survive; Git does not promise worktree-list ordering across platforms")
	originalBindings := branchesHeldByWorktrees
	branchesHeldByWorktrees = func(path string) (map[string][]string, error) {
		held, err := originalBindings(path)
		if err == nil && len(held[branch]) == 2 {
			held[branch] = []string{livePath, archived.GetWorktreePath()}
		}
		return held, err
	}
	t.Cleanup(func() { branchesHeldByWorktrees = originalBindings })

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "incoming",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "the live holder must refuse reuse even when an archived holder is listed after it")
	assert.Nil(t, renamed, "a refusal must happen before the archived rename moves the live lane's HEAD")
	assert.Equal(t, "incoming", archived.Title)
	assert.Contains(t, err.Error(), liveTitle, "the refusal must name the live lane among the multiple holders")
	head, headErr := exec.Command("git", "-C", livePath, "symbolic-ref", "--short", "HEAD").Output()
	require.NoError(t, headErr)
	assert.Equal(t, branch, strings.TrimSpace(string(head)), "the live lane's HEAD must remain on its original branch")
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

	release, err := manager.reserveLocalRestoreBranch(repoID, archived.Title, archived, true)
	if release != nil {
		release()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), actualBranch)
	assert.Contains(t, err.Error(), live.Title)
	assert.NotContains(t, err.Error(), manager.branchForTitle(archived.Title),
		"the cached archived branch must not decide admission after a manual checkout")
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

	releaseFirst, err := manager.reserveLocalRestoreBranch(repoID, first.Title, first, true)
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
		release, reserveErr := manager.reserveLocalRestoreBranch(repoID, second.Title, second, true)
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

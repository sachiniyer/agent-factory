package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteproject_refresh_fence_test.go covers the #2549/#2947 window that
// refreshLocked and the findSessionByStableID disk-rebuild register cross: a
// live, non-archived row that lands on instances.json between DeleteProject's
// pre-deregister re-check and its config.DeregisterProject call escapes both
// the re-check and the project-delete fence (neither refresh nor the disk
// rebuild consults m.projectDeletes / projectDeleteSeq the way reserveCreate
// does), so without the post-deregister re-scan the delete reports success
// while a live session survives in a now-deregistered project.
//
// The races are driven deterministically through the
// deleteProjectPreDeregisterHookForTest seam (which parks the delete inside the
// re-check→deregister gap) and the fromInstanceDataForRefresh seam (which
// materializes a freshly-written on-disk row into m.instances, exactly the
// 1 Hz poll's behavior, without needing a real worktree/tmux).

// seedUnregisteredRow writes one row to repoID's instances.json WITHOUT
// registering an in-memory instance — modelling a live row that exists on disk
// but is absent from m.instances (a peer agent-factory process wrote it, or a
// row that was unloadable at an earlier refresh just healed). Returns the
// instance the refresh will materialize it as, built with the supplied status
// (and, for archival-readiness, a real worktree when archivableBacked). The
// fromInstanceDataForRefresh override installed by the test returns that
// instance so RefreshInstances() deterministically tracks the row.
func seedUnregisteredRow(t *testing.T, repoID, repoPath, title string, status session.Status, archivableBacked bool) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	if archivableBacked {
		wtPath := filepath.Join(filepath.Dir(repoPath), "wt-"+sanitizeArchiveTitle(title))
		branch := "af/" + sanitizeArchiveTitle(title)
		out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput()
		require.NoError(t, err, string(out))
		require.NoError(t, os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644))
		gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, wtPath, title, branch, "", false, true)
		require.NoError(t, err)
		inst.SetGitWorktreeForTest(gw)
	}
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(status)
	seeded, err := json.Marshal([]session.InstanceData{
		{ID: inst.ID, Title: title, Path: repoPath, Status: status},
	})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))
	return inst
}

// installRefreshMaterializer overrides fromInstanceDataForRefresh to return
// inst for every on-disk row refresh materializes, so a freshly-seeded row is
// tracked in m.instances as a live (or archived) Instance without a real
// tmux/worktree build — the same shape the 1 Hz poll produces. Restored on
// cleanup.
func installRefreshMaterializer(t *testing.T, inst *session.Instance) {
	t.Helper()
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		return inst, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })
}

// runDeleteParkedAtDeregister starts DeleteProject in a goroutine and parks it
// inside the re-check→deregister window via
// deleteProjectPreDeregisterHookForTest. It returns a channel that closes when
// the delete has reached the deregister (re-check passed, m.mu released) and a
// release func that lets the delete proceed to the durable deregister + the
// post-deregister re-scan. The hook is cleared on cleanup.
func runDeleteParkedAtDeregister(t *testing.T, manager *Manager, req DeleteProjectRequest) (<-chan struct{}, func() <-chan deleteProjectCall) {
	t.Helper()
	reached := make(chan struct{})
	release := make(chan struct{})
	done := make(chan deleteProjectCall, 1)
	deleteProjectPreDeregisterHookForTest = func() {
		close(reached)
		<-release
	}
	t.Cleanup(func() { deleteProjectPreDeregisterHookForTest = nil })
	go func() {
		res, err := manager.DeleteProject(req)
		done <- deleteProjectCall{result: res, err: err}
	}()
	releaseAndAwait := func() <-chan deleteProjectCall {
		close(release)
		return done
	}
	return reached, releaseAndAwait
}

type deleteProjectCall struct {
	result DeleteProjectResult
	err    error
}

// TestDeleteProject_RefreshMaterializationDuringDeregisterIsFailClosed is the
// deterministic regression for the window: a live, non-archived row written to
// instances.json + RefreshInstances() lands in m.instances between the
// pre-deregister re-check and config.DeregisterProject, escape both, and
// WITHOUT the post-deregister re-scan the delete would report success while a
// live session survives in a now-deregistered project. With the fix, the
// post-deregister re-scan sees the materialized row and the delete returns a
// retryable error naming the surviving session.
func TestDeleteProject_RefreshMaterializationDuringDeregisterIsFailClosed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// No sessions exist; the delete's archive loop is empty and its
	// pre-deregister re-check passes. Park it inside the re-check→deregister
	// window.
	reached, releaseAndAwait := runDeleteParkedAtDeregister(t, manager, DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	<-reached

	// While parked, a peer process (or a healing row from an earlier
	// unloadable refresh) writes a LIVE row to disk and the daemon's refresh
	// materializes it into m.instances. refreshLocked consults neither the
	// fence map nor its counter, so it cannot self-refuse the way a create
	// would.
	liveInst := seedUnregisteredRow(t, repoID, repoPath, "orphan", session.Running, false)
	installRefreshMaterializer(t, liveInst)
	require.NoError(t, manager.RefreshInstances())

	// The live row is now tracked — the exact bug condition.
	manager.mu.Lock()
	tracked := manager.instances[daemonInstanceKey(repoID, "orphan")]
	manager.mu.Unlock()
	require.NotNil(t, tracked, "refresh materialized the live row into m.instances")
	require.NotEqual(t, session.LiveArchived, tracked.GetLiveness())

	// Release the delete; it deregisters, then the post-deregister re-scan
	// sees the live row and returns a retryable error.
	out := <-releaseAndAwait()
	require.Error(t, out.err, "a delete must NOT report success while a live session survives in a deregistered project")
	assert.Contains(t, out.err.Error(), "materialized for this project during its deregister")
	assert.Contains(t, out.err.Error(), "delete the project again to archive it and finish")
	assert.True(t, out.result.Deregistered, "the durable deregister already completed before the re-scan caught the escape")

	// The surviving session stays live and visible (snapshot/ArchiveSession
	// resolve m.instances regardless of registration) so the user can archive
	// it individually or by re-running DeleteProject — the remediation the
	// error message points at.
	manager.mu.Lock()
	survivor := manager.instances[daemonInstanceKey(repoID, "orphan")]
	manager.mu.Unlock()
	require.NotNil(t, survivor, "the surviving session must stay tracked for the retry")
	assert.NotEqual(t, session.LiveArchived, survivor.GetLiveness(), "the survivor must not be silently archived")
}

// TestDeleteProject_PostDeregisterReScanIgnoresArchivedRows verifies the
// post-deregister re-scan does NOT false-positive on a row that materialized as
// already-archived (a restorable archive the delete never needed to touch):
// repoSessionTitlesLocked excludes LiveArchived, so an archived row landing in
// m.instances during the window is not an escape and the delete reports
// success.
func TestDeleteProject_PostDeregisterReScanIgnoresArchivedRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	reached, releaseAndAwait := runDeleteParkedAtDeregister(t, manager, DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	<-reached

	// A row that materializes as LiveArchived is not a live session the delete
	// owed an archive — it is the restorable state the delete preserves.
	archivedInst := seedUnregisteredRow(t, repoID, repoPath, "restorable", session.Archived, false)
	installRefreshMaterializer(t, archivedInst)
	require.NoError(t, manager.RefreshInstances())

	manager.mu.Lock()
	require.Equal(t, session.LiveArchived, manager.instances[daemonInstanceKey(repoID, "restorable")].GetLiveness())
	manager.mu.Unlock()

	out := <-releaseAndAwait()
	require.NoError(t, out.err, "an archived row materializing during the deregister is not an escape")
	assert.True(t, out.result.Deregistered, "the deregister must complete when no live session escapes")
}

// TestDeleteProject_PostDeregisterReScanNoEscapeSucceeds verifies the
// post-deregister re-san does not false-positive when nothing materialized
// during the window — the common registered-but-sessionless delete. The hook
// is still installed and released immediately to prove the re-scan path runs
// and finds nothing.
func TestDeleteProject_PostDeregisterReScanNoEscapeSucceeds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	reached, releaseAndAwait := runDeleteParkedAtDeregister(t, manager, DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	<-reached
	// Nothing is seeded/refreshed: the window is empty, the re-scan finds nothing.
	out := <-releaseAndAwait()
	require.NoError(t, out.err)
	assert.True(t, out.result.Deregistered, "a registered, sessionless project deregisters cleanly")
	projects, err := config.ListProjects()
	require.NoError(t, err)
	assert.Empty(t, projects, "the durable registry record is gone")
}

// TestDeleteProject_RetryAfterRefreshMaterializationEscapeFinishes is the
// end-to-end remediation: after the post-deregister re-scan refuses the first
// delete, re-running DeleteProject for the same repo/path archives the
// materialized live session (its real worktree is in place) and finishes.
// This is the recovery the fail-closed error message points the user at.
func TestDeleteProject_RetryAfterRefreshMaterializationEscapeFinishes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// A real archivable row that starts disk-only (not in m.instances), so
	// the first delete's archive loop is empty and its pre-deregister
	// re-check passes — then refresh materializes it during the window.
	liveInst := seedUnregisteredRow(t, repoID, repoPath, "orphan", session.Ready, true)
	installRefreshMaterializer(t, liveInst)

	reached, releaseAndAwait := runDeleteParkedAtDeregister(t, manager, DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	<-reached
	require.NoError(t, manager.RefreshInstances())
	firstOut := <-releaseAndAwait()
	require.Error(t, firstOut.err, "the first delete must refuse when the live row escapes the re-check")
	assert.True(t, firstOut.result.Deregistered)

	// The hook was only for the first delete's window. Drop it before the
	// retry so the retry runs the ordinary path (and does not re-fire the
	// parked-window closure, which is not re-entrant).
	deleteProjectPreDeregisterHookForTest = nil

	// The retry re-resolves the repo by path (the user's real git repo is
	// untouched), re-fences, archives the now-visible live session through its
	// real worktree, and finishes — the durable record is already gone, so the
	// retry's deregister is a no-op.
	retryResult, retryErr := manager.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	require.NoError(t, retryErr, "a retry must finish by archiving the surviving session")
	require.Len(t, retryResult.Archived, 1, "the retry archives the formerly-escaped row")
	assert.Equal(t, "orphan", retryResult.Archived[0].Title)
	assert.False(t, retryResult.Deregistered, "the registry record was already removed by the first delete")

	manager.mu.Lock()
	archived := manager.instances[daemonInstanceKey(repoID, "orphan")]
	manager.mu.Unlock()
	require.NotNil(t, archived, "the archived row is preserved (restorable)")
	assert.Equal(t, session.LiveArchived, archived.GetLiveness())
}

// TestDeleteProject_E2ERealPollLoopRefreshDuringDeregister is the end-to-end
// race against the real 1 Hz poll loop (startInstancePollLoop, the production
// goroutine), not a manually-called RefreshInstances. It exercises the bug's
// exact production actor class — a poll tick that refreshes from disk while a
// DeleteProject is parked inside the re-check→deregister window — and asserts
// the post-deregister re-scan catches the escape and returns a retryable error
// rather than reporting success over a live session in a deregistered project.
//
// This covers the plan's e2e procedure: a live row written to instances.json
// (modelling a peer agent-factory process or a healing row) is materialized by
// the real poll loop's RefreshInstances() while the delete is in flight, the
// delete catches it after the deregister, and the surviving session stays
// visible in the session list (Snapshot serves m.instances regardless of
// registration).
func TestDeleteProject_E2ERealPollLoopRefreshDuringDeregister(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Seed the live row on disk BEFORE anything starts: at delete start it
	// must be disk-only (absent from m.instances) so the delete's phase-1 gate
	// sees nothing, then materialize into m.instances DURING the parked
	// re-check→deregister window. The poll loop's RefreshInstances reads the
	// fromInstanceDataForRefresh package var on every tick, so the override
	// must be installed BEFORE the loop starts (installing it after would race
	// the loop's read against the test's write). The override is gated on a
	// channel so it returns a transient failure (row stays disk-only) until
	// the delete is parked, then returns the live instance (the row
	// materializes into the window — exactly the bug's triggering condition).
	liveInst := seedUnregisteredRow(t, repoID, repoPath, "late-arrival", session.Running, false)
	materializeGate := make(chan struct{})
	prev := fromInstanceDataForRefresh
	fromInstanceDataForRefresh = func(d session.InstanceData, rc *session.RestoreContext) (*session.Instance, error) {
		select {
		case <-materializeGate:
			// The delete is parked in the re-check→deregister window; materialize the row.
		default:
			return nil, fmt.Errorf("transient restore failure")
		}
		return liveInst, nil
	}
	t.Cleanup(func() { fromInstanceDataForRefresh = prev })

	// The real poll loop: a goroutine running RefreshInstances() (and the rest
	// of the maintenance passes) on a 10ms tick so the race fires reliably
	// within the parked window. stopCh halts it at test end.
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	startInstancePollLoop(manager, 10*time.Millisecond, stopCh, &wg)
	t.Cleanup(func() { close(stopCh); wg.Wait() })

	// Park the delete inside the re-check→deregister window. The delete's
	// phase-1 gate sees no row (the materializer is still gating to failure,
	// so the row is disk-only and absent from m.instances).
	reached, releaseAndAwait := runDeleteParkedAtDeregister(t, manager, DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath})
	<-reached

	// Open the gate: the next poll tick's RefreshInstances materializes the
	// on-disk live row into m.instances DURING the parked window — the bug's
	// exact production actor (a peer process / healing row / start-skipped
	// repo that repaired).
	close(materializeGate)

	// Wait for the poll loop to materialize the row (a tick or two at 10ms).
	require.Eventually(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.instances[daemonInstanceKey(repoID, "late-arrival")] != nil
	}, 2*time.Second, 5*time.Millisecond, "the real poll loop must materialize the on-disk live row")

	// Release the delete; the post-deregister re-scan sees the live row and
	// returns a retryable error.
	out := <-releaseAndAwait()
	require.Error(t, out.err, "the delete must not report success while a live session survives in a deregistered project")
	assert.Contains(t, out.err.Error(), "materialized for this project during its deregister")
	assert.Contains(t, out.err.Error(), "delete the project again to archive it and finish")
	assert.True(t, out.result.Deregistered, "the durable deregister completed before the re-scan caught the escape")

	// The surviving session stays live and visible in the Snapshot (the TUI/
	// session switcher's data source, served from m.instances regardless of
	// registration) so the user can act on it.
	found := false
	for _, inst := range manager.InstancesSnapshot() {
		if inst != nil && inst.Title == "late-arrival" && inst.GetLiveness() != session.LiveArchived {
			found = true
			break
		}
	}
	require.True(t, found, "the surviving live session must stay visible in the session list/switcher")

	// Confirm the registration record is gone (the durable deregister ran).
	projects, err := config.ListProjects()
	require.NoError(t, err)
	assert.Empty(t, projects, "the durable registry record was removed by the first delete")
}

// TestDeleteProject_ControlServerRPCSurfacesRefreshMaterializationEscape
// confirms the retryable error the post-deregister re-scan returns reaches the
// CLI/web caller through the control-server RPC envelope — the path `af
// projects delete` and the web delete button call. The control server's
// DeleteProject handler invokes manager.DeleteProject and returns its error
// verbatim (`if err != nil { return err }`), so a caller that asks the daemon
// to delete a project whose live session materialized during the deregister
// receives the retryable error (and the response stays non-OK), rather than
// the success envelope (`ok: true`).
func TestDeleteProject_ControlServerRPCSurfacesRefreshMaterializationEscape(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	registerTestProject(t, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	repoID := repo.ID

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Drive the delete through the control-server RPC — the same path the
	// CLI's `af projects delete` takes — and park it inside the
	// re-check→deregister window via the hook.
	reached := make(chan struct{})
	release := make(chan struct{})
	deleteProjectPreDeregisterHookForTest = func() {
		close(reached)
		<-release
	}
	t.Cleanup(func() { deleteProjectPreDeregisterHookForTest = nil })

	server := &controlServer{manager: manager}
	rpcDone := make(chan struct {
		resp DeleteProjectResponse
		err  error
	}, 1)
	go func() {
		var resp DeleteProjectResponse
		err := server.DeleteProject(DeleteProjectRequest{RepoID: repoID, RepoPath: repoPath}, &resp)
		rpcDone <- struct {
			resp DeleteProjectResponse
			err  error
		}{resp, err}
	}()
	<-reached

	// A live row materializes into m.instances during the parked window.
	liveInst := seedUnregisteredRow(t, repoID, repoPath, "escapee", session.Running, false)
	installRefreshMaterializer(t, liveInst)
	require.NoError(t, manager.RefreshInstances())

	// Release the delete; the post-deregister re-scan sees the live row and
	// the RPC returns the retryable error (response stays non-OK).
	close(release)
	out := <-rpcDone
	require.Error(t, out.err, "the control-server RPC must surface the retryable error, not a success envelope")
	assert.Contains(t, out.err.Error(), "materialized for this project during its deregister")
	assert.Contains(t, out.err.Error(), "the project's registry record was removed but a live session survives",
		"the RPC error must convey that the durable record is gone and a live session survives")
	assert.Contains(t, out.err.Error(), "delete the project again to archive it and finish")
	assert.False(t, out.resp.OK, "the response envelope must not be OK when the delete refused")
}

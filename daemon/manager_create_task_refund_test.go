package daemon

// Regression suite for the watch-task rate-slot refund on reserveCreate pre-flight
// failures. reserveCreate performs three pre-flight operations whose FIRST call (in
// DeliverPromptWithStatus) is wrapped notAttempted but whose SECOND call (inside
// reserveCreate) used to return a plain error, so the watch delivery path's
// isNotAttemptedErr check returned false and the reserved rate slot was held for the
// full 60s window. The fix wraps those three returns with notAttempted for
// task-originated requests, mirroring the in-function projectDeleteRefusal precedent.
// These tests pin each site directly, plus the end-to-end refund on both delivery
// arms and the over-refund boundary.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transientRepoResolutionFail overrides the reserveCreate repo-resolution seam to
// fail for repoPath, modeling the blip the bug needs: the first config.RepoFromPath
// (delivery.go / defaultProgramFor) succeeds while the second (inside reserveCreate)
// fails. Restored on cleanup.
func transientRepoResolutionFail(t *testing.T, repoPath string) {
	t.Helper()
	orig := repoFromPathForCreate
	repoFromPathForCreate = func(path string) (*config.RepoContext, error) {
		if path == repoPath {
			return nil, fmt.Errorf("transient git outage: rev-parse failed")
		}
		return orig(path)
	}
	t.Cleanup(func() { repoFromPathForCreate = orig })
}

// corruptRepoInstances plants a corrupt instances.json for repoID so the targeted
// loadRepoInstanceData(repo.ID) in reserveCreate fails while refreshLocked skips the
// same file (migration skips errInstancesSchemaContent and LoadAllRepoInstances
// reports it as a skip, not an error).
func corruptRepoInstances(t *testing.T, repoID string) {
	t.Helper()
	require.NoError(t, config.LoadState().SaveInstances(repoID, json.RawMessage("{not valid json")))
}

// unreadableRepoInstances replaces the repo's instances.json with a DIRECTORY so
// MigrateAllRepoInstancesForDaemonLoad — which os.ReadFile's it and refuses HARD on
// a read failure (unlike the skip-and-warn for corrupt content) — fails, making
// refreshLocked return an error. This isolates the second pre-flight return
// (refreshLocked): the failure provably precedes loadRepoInstanceData.
func unreadableRepoInstances(t *testing.T, repoID string) {
	t.Helper()
	path, err := config.RepoInstancesPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.RemoveAll(path))
	require.NoError(t, os.Mkdir(path, 0o755))
}

// seedWatchTaskForManager adds an enabled watch task pointing at the manager's own
// repo. The target session is NOT seeded on disk, so a non-empty target drives the
// auto-create arm (deliverPromptForTask) and an empty target drives the fresh-per-run
// create arm (createSessionForTask).
func seedWatchTaskForManager(t *testing.T, taskID, projectPath, target string) {
	t.Helper()
	if err := task.AddTask(task.Task{
		ID:            taskID,
		Name:          "repro-watch",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: target,
		ProjectPath:   projectPath,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed watch task: %v", err)
	}
}

// flattenDeliverPrompt swaps deliverPromptForTask for a stub that calls the real
// manager.DeliverPrompt and reconstitutes the error from TEXT only — exactly what
// net/rpc does on the daemon's own control socket — so the re-mint at taskrun.go:143
// is exercised faithfully.
func flattenDeliverPrompt(t *testing.T, manager *Manager) {
	t.Helper()
	orig := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		status, err := manager.DeliverPrompt(req)
		if err == nil {
			return status, nil
		}
		return "", fmt.Errorf("%s", err.Error())
	}
	t.Cleanup(func() { deliverPromptForTask = orig })
}

// flattenCreateSession is the create-arm counterpart: it swaps createSessionForTask
// for a stub that calls the real manager.reserveCreate and flattens the error to
// text, exercising the re-mint at taskrun.go:98.
func flattenCreateSession(t *testing.T, manager *Manager) {
	t.Helper()
	orig := createSessionForTask
	createSessionForTask = func(req CreateSessionRequest) (*session.InstanceData, error) {
		_, _, release, _, err := manager.reserveCreate(req)
		if err != nil {
			return nil, fmt.Errorf("%s", err.Error())
		}
		release()
		t.Fatalf("createSessionForTask stub: expected the pre-flight failure, but reserveCreate succeeded")
		return nil, nil
	}
	t.Cleanup(func() { createSessionForTask = orig })
}

// TestRepro_ReserveCreateRepoFromPathFailureIsNotAttempted pins the first pre-flight
// return: a TASK create whose second repo resolution fails during a transient git
// blip provably reserved nothing, so it must refund. An ordinary client create keeps
// its plain error — the task-origin gate must not over-refund.
func TestRepro_ReserveCreateRepoFromPathFailureIsNotAttempted(t *testing.T) {
	for _, tc := range []struct {
		name string
		task bool
	}{
		{"task create (target empty)", true},
		{"ordinary client create", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			transientRepoResolutionFail(t, repoPath)
			req := CreateSessionRequest{Title: "worker", RepoPath: repoPath, Program: "claude"}
			if tc.task {
				req.TaskID = "task-rp-01"
				req.TaskOrigin = true
				req.TaskRepoID = repoID
			}
			_, _, release, _, err := manager.reserveCreate(req)
			if err == nil {
				release()
				t.Fatal("expected a pre-flight repo-resolution failure")
			}
			if tc.task {
				assert.True(t, isNotAttemptedErr(err), "a task pre-flight repo-resolution failure must refund the rate slot")
				assert.Contains(t, err.Error(), notDeliveredMarker, "the marker must survive net/rpc flattening")
			} else {
				assert.False(t, isNotAttemptedErr(err), "an ordinary client create must keep its plain error")
				assert.NotContains(t, err.Error(), notDeliveredMarker, "ordinary client errors must not carry the watch marker")
			}
		})
	}
}

// TestRepro_ReserveCreateRefreshFailureIsNotAttempted pins the second pre-flight
// return (refreshLocked): a TASK create whose locked refresh fails (here a
// daemon-load migration HARD refusal on an unreadable per-repo instances file)
// provably precedes the reservation commit, so it must refund. An ordinary client
// create keeps its plain error.
func TestRepro_ReserveCreateRefreshFailureIsNotAttempted(t *testing.T) {
	for _, tc := range []struct {
		name string
		task bool
	}{
		{"task create (target empty)", true},
		{"ordinary client create", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			unreadableRepoInstances(t, repoID)
			req := CreateSessionRequest{Title: "worker", RepoPath: repoPath, Program: "claude"}
			if tc.task {
				req.TaskID = "task-refresh-01"
				req.TaskOrigin = true
				req.TaskRepoID = repoID
			}
			_, _, release, _, err := manager.reserveCreate(req)
			if err == nil {
				release()
				t.Fatal("expected a pre-flight refresh failure")
			}
			if tc.task {
				assert.True(t, isNotAttemptedErr(err), "a task pre-flight refresh failure must refund the rate slot")
				assert.Contains(t, err.Error(), notDeliveredMarker)
			} else {
				assert.False(t, isNotAttemptedErr(err), "an ordinary client create must keep its plain error")
			}
		})
	}
}

// TestRepro_ReserveCreateLoadFailureIsNotAttempted pins the third pre-flight return
// (loadRepoInstanceData): a TASK create whose on-disk instance load fails during a
// transient disk/json blip provably precedes the reservation commit, so it must
// refund. An ordinary client create keeps its plain error.
func TestRepro_ReserveCreateLoadFailureIsNotAttempted(t *testing.T) {
	for _, tc := range []struct {
		name string
		task bool
	}{
		{"task create (target empty)", true},
		{"ordinary client create", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			corruptRepoInstances(t, repoID)
			req := CreateSessionRequest{Title: "worker", RepoPath: repoPath, Program: "claude"}
			if tc.task {
				req.TaskID = "task-load-01"
				req.TaskOrigin = true
				req.TaskRepoID = repoID
			}
			_, _, release, _, err := manager.reserveCreate(req)
			if err == nil {
				release()
				t.Fatal("expected a pre-flight instance-load failure")
			}
			if tc.task {
				assert.True(t, isNotAttemptedErr(err), "a task pre-flight instance-load failure must refund the rate slot")
				assert.Contains(t, err.Error(), notDeliveredMarker)
			} else {
				assert.False(t, isNotAttemptedErr(err), "an ordinary client create must keep its plain error")
			}
		})
	}
}

// TestRepro_WatcherRefundsRateSlotOnAutoCreatePreflight is the end-to-end refund on
// the live path for the target!="" arm: the real deliverWatchEvent -> deliverTaskPrompt
// path with deliverPromptForTask flattened exactly as net/rpc flattens it. A
// pre-flight auto-create failure must re-mint notAttempted from the wire marker and
// refund the slot the watcher reserved.
func TestRepro_WatcherRefundsRateSlotOnAutoCreatePreflight(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	transientRepoResolutionFail(t, repoPath)
	seedWatchTaskForManager(t, "repro-auto-live", repoPath, "absent-worker")
	flattenDeliverPrompt(t, manager)

	w := newRateSlotWatcher(t, "repro-auto-live", deliverWatchEvent)
	close(w.stopCh)
	w.handleEvent("new issue #9", &tailBuffer{})

	assert.Equal(t, 0, spentSlots(w), "a pre-flight auto-create failure must refund the rate slot")
	assert.Equal(t, 1, w.queue.pendingCount(), "a failed delivery is queued for replay")
}

// TestRepro_WatcherRefundsRateSlotOnCreatePreflight is the create-arm (target=="")
// counterpart. It drives the real createSessionForTask -> reserveCreate path with the
// error flattened as net/rpc does, so the re-mint at taskrun.go:98 fires and the
// watcher refunds. The create-arm re-mint has no other coverage in the package's
// rate-slot suite (the #2501 regression uses the auto-create arm), so this pins it.
func TestRepro_WatcherRefundsRateSlotOnCreatePreflight(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	transientRepoResolutionFail(t, repoPath)
	seedWatchTaskForManager(t, "repro-create-live", repoPath, "")
	flattenCreateSession(t, manager)

	w := newRateSlotWatcher(t, "repro-create-live", deliverWatchEvent)
	close(w.stopCh)
	w.handleEvent("new issue #9", &tailBuffer{})

	assert.Equal(t, 0, spentSlots(w), "a pre-flight create failure must refund the rate slot")
	assert.Equal(t, 1, w.queue.pendingCount(), "a failed delivery is queued for replay")
}

// TestRepro_ReserveCreateGenuineConflictStaysChargedForTask is the non-regression
// guard against over-refund: a TASK create that fails on a GENUINE title collision
// (a live session already owns the title) is a persistent config conflict, not a
// transient pre-flight blip, so it must stay charged — the fix wraps only the
// three pre-flight returns, not the post-admission conflict returns.
func TestRepro_ReserveCreateGenuineConflictStaysChargedForTask(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "worker", session.NewFakeBackend(), true, session.Ready)

	_, _, release, _, err := manager.reserveCreate(CreateSessionRequest{
		Title:      "worker",
		RepoPath:   repoPath,
		Program:    "claude",
		TaskID:     "task-conf-01",
		TaskOrigin: true,
		TaskRepoID: repoID,
	})
	if err == nil {
		release()
		t.Fatal("expected a genuine title-collision refusal")
	}
	assert.False(t, isNotAttemptedErr(err), "a genuine title collision must stay charged (not refundable)")
	assert.NotContains(t, err.Error(), notDeliveredMarker, "a genuine conflict must not carry the watch marker")
}

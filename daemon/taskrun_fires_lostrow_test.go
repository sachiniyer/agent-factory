package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// These tests pin the LiveLost arm of reconcilePendingTaskFire's adoption
// guard. The boot loader defers certain interrupted-create rows to the
// lost-restore loop by loading them Liveness == LiveLost WITHOUT respawning
// and WITHOUT pre-stamping TaskRunInterrupted — a deferred restore (worktree
// missing/unreachable, socket unverifiable under a possibly-live server, or a
// staged replacement conversation not made durable). The durable interrupt
// stamp only lands once lost-restore runs git worktree add + tmux spawn,
// which races this replay's file read with no ordering barrier.
//
// Before the fix, a matched LiveLost row had no arm in the adoption switch
// and fell through to the default "started": the occurrence was stamped
// started and its marker cleared before the lost-restore respawn ever
// received the prompt — the cron occurrence was silently consumed. The fix
// treats such a row the same way the #4222 TaskRunInterrupted skip already
// does: not the fire's delivered product, so the fire stays owed and falls
// through to a fresh delivery.

// TestReconcilePendingTaskFire_LiveLostRowIsNotAdopted is the direct
// reproduction from the bug report: a matched row whose Liveness is LiveLost
// (and TaskRunInterrupted still false) must NOT be adopted as the fire's
// delivered product. The reconcile must report the fire still owed
// (done=false, err=nil), the marker must survive, and the row itself must be
// left untouched for its own lost-restore recovery.
func TestReconcilePendingTaskFire_LiveLostRowIsNotAdopted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7200", repoPath)

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	intendedAt := time.Now().Add(-time.Minute)
	seeded := session.InstanceData{
		ID:                 "inst-lost-1",
		TaskID:             "ab0r7200",
		Title:              "nightly-lost",
		Path:               repoPath,
		Program:            "claude",
		Status:             session.Ready,
		Liveness:           session.LiveLost,
		TaskRunInterrupted: false, // the boot loader's deferred-restore path does not pre-stamp
		CreatedAt:          intendedAt.Add(2 * time.Second),
		UpdatedAt:          intendedAt.Add(2 * time.Second),
	}
	if err := appendInstanceData(repo.ID, seeded); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	fire := &taskFireMarker{
		TaskID:     "ab0r7200",
		IntendedAt: intendedAt,
		Title:      "nightly-lost",
		InstanceID: "inst-lost-1",
	}
	seedPendingFire(t, *fire)

	tk, err := task.GetTask("ab0r7200")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	done, rerr := reconcilePendingTaskFire(tk, fire)
	if rerr != nil {
		t.Fatalf("reconcilePendingTaskFire: %v", rerr)
	}
	if done {
		t.Fatal("a LiveLost row must not be adopted as the fire's delivered product; the occurrence is still owed (done=true)")
	}
	if !markerExists(t, "ab0r7200") {
		t.Fatal("the marker must survive so the still-owed occurrence can deliver fresh")
	}
	// The row must be untouched: its identity, liveness, and interrupt flag are
	// all owned by the lost-restore loop, not the fire reconcile.
	rows, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "inst-lost-1" {
		t.Fatalf("the LiveLost row must survive unmutated; got rows = %+v", rows)
	}
	if rows[0].Liveness != session.LiveLost {
		t.Fatalf("row Liveness = %v, want LiveLost (lost-restore owns it)", rows[0].Liveness)
	}
	if rows[0].TaskRunInterrupted {
		t.Fatal("reconcile must not stamp TaskRunInterrupted on a row it skipped — lost-restore owns that stamp")
	}
}

// TestReconcilePendingTaskFire_LiveLostPrecedenceOverOtherFlags pins that a
// LiveLost row is skipped regardless of the other adoption-switch flags
// (UserKilled, StartupStateUnknown). Those arms were never legitimate for a
// dead runtime queued for lost-restore respawn either — a row whose runtime
// is gone is not the fire's delivered product no matter what teardown-shaped
// flag it also carries, and the LiveLost skip must take precedence so the
// occurrence stays owed. Each subcase owns its own task so rows do not
// cross-match by task-association in the shared repo.
func TestReconcilePendingTaskFire_LiveLostPrecedenceOverOtherFlags(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	intendedAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name               string
		taskID             string
		instanceID         string
		userKilled         bool
		startupUnknown     bool
		limitReached       bool
		taskRunInterrupted bool
		wantAdopted        bool // LiveLost precedence guard: only the limit-reached control adopts
	}{
		{name: "live-lost alone", taskID: "ab0r7210", instanceID: "inst-lost-10"},
		{name: "live-lost and user-killed", taskID: "ab0r7211", instanceID: "inst-lost-11", userKilled: true},
		{name: "live-lost and startup-unknown", taskID: "ab0r7212", instanceID: "inst-lost-12", startupUnknown: true},
		{name: "live-lost and task-run-interrupted", taskID: "ab0r7213", instanceID: "inst-lost-13", taskRunInterrupted: true},
		// Control: a LiveLimitReached (NOT LiveLost) row is still adopted
		// (parked) — the fix only added the LiveLost arm. This guards against
		// an over-broad skip that would regress limit-parked adoption.
		{name: "limit-reached (control)", taskID: "ab0r7214", instanceID: "inst-limit-14", limitReached: true, wantAdopted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedCronTask(t, tc.taskID, repoPath)
			liveness := session.LiveLost
			if tc.limitReached {
				liveness = session.LiveLimitReached
			}
			if err := appendInstanceData(repo.ID, session.InstanceData{
				ID:                  tc.instanceID,
				TaskID:              tc.taskID, // own task: task-association match cannot reach other subcases' rows
				Title:               "nightly-" + tc.instanceID,
				Path:                repoPath,
				Program:             "claude",
				Status:              session.Ready,
				Liveness:            liveness,
				UserKilled:          tc.userKilled,
				StartupStateUnknown: tc.startupUnknown,
				TaskRunInterrupted:  tc.taskRunInterrupted,
				CreatedAt:           intendedAt.Add(2 * time.Second),
				UpdatedAt:           intendedAt.Add(2 * time.Second),
			}); err != nil {
				t.Fatalf("seed session row: %v", err)
			}
			fire := &taskFireMarker{
				TaskID:     tc.taskID,
				IntendedAt: intendedAt,
				Title:      "nightly-" + tc.instanceID,
				InstanceID: tc.instanceID,
			}
			seedPendingFire(t, *fire)

			tk, err := task.GetTask(tc.taskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			done, rerr := reconcilePendingTaskFire(tk, fire)
			if rerr != nil {
				t.Fatalf("reconcilePendingTaskFire: %v", rerr)
			}
			if done != tc.wantAdopted {
				t.Fatalf("done = %v, want %v (adopted=%v) for %s", done, tc.wantAdopted, tc.wantAdopted, tc.name)
			}
			if tc.wantAdopted {
				// The limit-reached control is adopted (parked): the marker
				// discharges — the occurrence is consumed honestly.
				if markerExists(t, tc.taskID) {
					t.Fatal("an adopted limit-reached row must discharge the marker")
				}
				return
			}
			if !markerExists(t, tc.taskID) {
				t.Fatal("a skipped LiveLost row must keep the marker — the occurrence is still owed")
			}
		})
	}
}

// TestRunTask_ReplayDoesNotAdoptLiveLostRow is the end-to-end regression
// mirroring TestRunTask_ReplayDoesNotAdoptAnInterruptedRow for the LiveLost
// shape: a create committed its row and bound the marker, the daemon died,
// and on restart the row's runtime routed through the deferred-restore arm —
// loaded LiveLost, no respawn, no pre-stamped interrupt. The pending marker
// is still owed its delivery; reconcile must not adopt the LiveLost row as
// the fire's product, and the replay must deliver the occurrence fresh under
// a new suffix instead of consuming it for nothing.
func TestRunTask_ReplayDoesNotAdoptLiveLostRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7202", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	// The deferred-restore shape: LiveLost, no TaskRunInterrupted pre-stamp.
	// Status carries Ready (lost-restore preserves the last composed status);
	// only the liveness axis names the deferred-restore state.
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                 "inst-lost-2",
		TaskID:             "ab0r7202",
		Title:              "nightly-lost-2",
		Path:               repoPath,
		Program:            "claude",
		Status:             session.Ready,
		Liveness:           session.LiveLost,
		TaskRunInterrupted: false,
		CreatedAt:          intendedAt.Add(2 * time.Second),
		UpdatedAt:          intendedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7202",
		IntendedAt: intendedAt,
		Title:      "nightly-lost-2",
		InstanceID: "inst-lost-2",
	})

	if err := runTaskDelivery("ab0r7202", task.ProjectExpectation{}, replayMarker("ab0r7202", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("a LiveLost row is not the fire's product — the still-owed occurrence must deliver fresh; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7202"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started (the fresh delivery's own run)", got)
	}
	if markerExists(t, "ab0r7202") {
		t.Fatal("the fresh delivery discharges the marker")
	}
	// The LiveLost row must survive the fresh delivery untouched: the leftover
	// scan's foreign-row title match declines to reap an identity this row
	// still owns, and lost-restore still owns its recovery.
	rows, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	var lost *session.InstanceData
	for i := range rows {
		if rows[i].ID == "inst-lost-2" {
			lost = &rows[i]
		}
	}
	if lost == nil {
		t.Fatal("the LiveLost row must survive the fresh delivery; lost-restore still owns it")
	}
	if lost.Liveness != session.LiveLost {
		t.Fatalf("the LiveLost row's liveness must be unchanged; got %v", lost.Liveness)
	}
	if lost.TaskRunInterrupted {
		t.Fatal("the row's TaskRunInterrupted flag must be unchanged by the delivery path")
	}
}

// TestReconcilePendingTaskFire_LiveLostMatchedByTaskAssociation pins the
// non-instance-id match path: when the marker carries no InstanceID (an
// older marker written before the create bound it), a LiveLost row is matched
// by task association + CreatedAt strictly after the intent, and must still
// be skipped. The instance-id path and the task-association path share the
// same adoption guard, so both must honor the LiveLost skip.
func TestReconcilePendingTaskFire_LiveLostMatchedByTaskAssociation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7203", repoPath)

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	intendedAt := time.Now().Add(-time.Minute)
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:        "inst-lost-3",
		TaskID:    "ab0r7203", // matched by task association, not instance id
		Title:     "nightly-lost-3",
		Path:      repoPath,
		Program:   "claude",
		Status:    session.Ready,
		Liveness:  session.LiveLost,
		CreatedAt: intendedAt.Add(2 * time.Second),
		UpdatedAt: intendedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	// No InstanceID: the reconcile must fall back to task-association matching.
	fire := &taskFireMarker{
		TaskID:     "ab0r7203",
		IntendedAt: intendedAt,
		Title:      "nightly-lost-3",
	}
	seedPendingFire(t, *fire)

	tk, err := task.GetTask("ab0r7203")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	done, rerr := reconcilePendingTaskFire(tk, fire)
	if rerr != nil {
		t.Fatalf("reconcilePendingTaskFire: %v", rerr)
	}
	if done {
		t.Fatal("a task-associated LiveLost row must not be adopted; the occurrence is still owed")
	}
	if !markerExists(t, "ab0r7203") {
		t.Fatal("the marker must survive so the still-owed occurrence can deliver fresh")
	}
}

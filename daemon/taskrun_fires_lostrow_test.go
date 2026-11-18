package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// These tests pin how a pending cron fire reconciles a matched row that the boot
// loader parked Lost (#7518). A cron delivery sends the prompt, then appends the
// row, and only then stamps "started" and clears the marker. A daemon that dies
// in between leaves a prompted row and a pending marker, and the next boot may
// load that row Lost without respawning it. Lost is two different facts there:
//
//   - tmux confirmed the agent's session absent and the respawn was refused (its
//     worktree is gone). The prompt went to a process that no longer exists, so
//     the occurrence must deliver fresh, or it is consumed for nothing.
//   - the socket could not be verified under a possibly-live server. The agent
//     may be working on this very prompt, so delivering would start a second run
//     of the occurrence. The fire is held: nothing delivers, the marker stays,
//     and the row's own restore decides it.
//
// The loader records which one it saw in LostAgentGone (session/
// restore_deferred_lost_test.go pins that half). These seed it directly.

const lostRowTitle = "nightly-lost"

// lostFire seeds a cron task, the row its interrupted fire created, and the
// pending marker bound to that row. The row is Lost with its run in flight;
// shape adjusts it.
func lostFire(t *testing.T, taskID, repoPath string, intendedAt time.Time, shape func(*session.InstanceData)) string {
	t.Helper()
	seedCronTask(t, taskID, repoPath)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	row := session.InstanceData{
		ID:            "inst-" + taskID,
		TaskID:        taskID,
		Title:         lostRowTitle + "-" + taskID,
		Path:          repoPath,
		Program:       "claude",
		Liveness:      session.LiveLost,
		TaskRunActive: true,
		CreatedAt:     intendedAt.Add(2 * time.Second),
		UpdatedAt:     intendedAt.Add(2 * time.Second),
	}
	if shape != nil {
		shape(&row)
	}
	if err := appendInstanceData(repo.ID, row); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: taskID, IntendedAt: intendedAt, Title: row.Title, InstanceID: row.ID})
	return repo.ID
}

func proveAgentGone(d *session.InstanceData) { d.LostAgentGone = true }

// settleRow rewrites the seeded row the way its restore would leave it.
func settleRow(t *testing.T, repoID, taskID string, settle func(*session.InstanceData)) {
	t.Helper()
	row := findRow(t, repoID, "inst-"+taskID)
	settle(&row)
	if err := persistInstanceData(repoID, row); err != nil {
		t.Fatalf("persist settled row: %v", err)
	}
}

func findRow(t *testing.T, repoID, id string) session.InstanceData {
	t.Helper()
	rows, err := loadRepoInstanceData(repoID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("row %s is gone; got %+v", id, rows)
	return session.InstanceData{}
}

// rebound is a restore that reattached the still-running agent.
func rebound(d *session.InstanceData) {
	d.Liveness, d.LostAgentGone = session.LiveRunning, false
}

// respawned is a restore that replaced the agent without replaying its prompt.
func respawned(d *session.InstanceData) {
	d.Liveness, d.LostAgentGone = session.LiveRunning, false
	d.TaskRunActive, d.TaskRunInterrupted = false, true
}

func lastRun(t *testing.T, taskID string) *task.Task {
	t.Helper()
	got, err := task.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return got
}

func requireHeld(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errTaskFireRuntimeUnknown) || !errors.Is(err, errTaskFireReconcile) {
		t.Fatalf("the fire must be held as an undetermined reconcile; got %v", err)
	}
}

// (b) The missing-worktree shape #7518 reports: the agent is proven gone, so the
// occurrence delivers exactly once, under a new suffix, and the Lost row is left
// for its own rebuild.
func TestRunTask_ReplayDeliversOnceWhenTheLostAgentIsProvenGone(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	repoID := lostFire(t, "ab0r7520", repoPath, intendedAt, proveAgentGone)

	if err := runTaskDelivery("ab0r7520", task.ProjectExpectation{}, replayMarker("ab0r7520", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("a proven-gone agent never ran the occurrence, so it must deliver exactly once; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7520"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started (the fresh delivery's own run)", got)
	}
	if markerExists(t, "ab0r7520") {
		t.Fatal("the fresh delivery discharges the marker")
	}
	row := findRow(t, repoID, "inst-ab0r7520")
	if row.Liveness != session.LiveLost || !row.LostAgentGone || row.TaskRunInterrupted {
		t.Fatalf("the Lost row belongs to its own rebuild and must be left as it was; got %+v", row)
	}
}

// (a) The case the first version of this fix got wrong. The socket could not be
// verified, so the agent may still be running the prompt: zero new sessions, and
// the marker stays owed. A second look before the restore settles changes
// nothing, and does not rewrite the held status either.
func TestRunTask_ReplayHoldsALostRowWhoseAgentMayBeRunning(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	repoID := lostFire(t, "ab0r7521", repoPath, intendedAt, nil)

	requireHeld(t, runTaskDelivery("ab0r7521", task.ProjectExpectation{}, replayMarker("ab0r7521", intendedAt)))
	if len(*creates) != 0 {
		t.Fatalf("a second session would run the occurrence beside an agent that may still be running it; creates = %d", len(*creates))
	}
	if !markerExists(t, "ab0r7521") {
		t.Fatal("a held fire keeps its marker: the occurrence is still owed until the restore settles")
	}
	first := lastRun(t, "ab0r7521")
	if !strings.HasPrefix(first.LastRunStatus, "errored: "+errTaskFireReconcile.Error()) {
		t.Fatalf("LastRunStatus = %q, want the reconcile-failure status", first.LastRunStatus)
	}
	if !strings.Contains(first.LastRunStatus, lostRowTitle+"-ab0r7521") {
		t.Fatalf("the held status must name the session it waits on; got %q", first.LastRunStatus)
	}

	requireHeld(t, runTaskDelivery("ab0r7521", task.ProjectExpectation{}, replayMarker("ab0r7521", intendedAt)))
	if len(*creates) != 0 {
		t.Fatalf("looking again must not deliver while nothing has settled; creates = %d", len(*creates))
	}
	if again := lastRun(t, "ab0r7521"); again.LastRunAt == nil || first.LastRunAt == nil || !again.LastRunAt.Equal(*first.LastRunAt) {
		t.Fatalf("the same held verdict is not a new run outcome; LastRunAt moved from %v to %v", first.LastRunAt, again.LastRunAt)
	}
	row := findRow(t, repoID, "inst-ab0r7521")
	if row.Liveness != session.LiveLost || row.TaskRunInterrupted {
		t.Fatalf("a held reconcile must leave the row to its restore; got %+v", row)
	}
}

// The no-instance-id match (a marker the create never bound) reaches the same
// arm.
func TestReconcilePendingTaskFire_HoldsALostRowMatchedByTaskAssociation(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	intendedAt := time.Now().Add(-time.Minute)
	lostFire(t, "ab0r7522", repoPath, intendedAt, nil)
	fire := &taskFireMarker{TaskID: "ab0r7522", IntendedAt: intendedAt, Title: lostRowTitle + "-ab0r7522"}
	seedPendingFire(t, *fire)

	done, err := reconcilePendingTaskFire(lastRun(t, "ab0r7522"), fire)
	requireHeld(t, err)
	if done || !markerExists(t, "ab0r7522") {
		t.Fatalf("a task-associated Lost row is held like a bound one; done=%v", done)
	}
}

// (c) The retained-failure classification outranks the Lost projection: an
// uncertain or killed row is consumed as aborted, never delivered fresh, even
// when the loader proved the agent gone.
func TestRunTask_ReplayKeepsRetainedFailurePrecedenceOverLost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		taskID string
		shape  func(*session.InstanceData)
	}{
		{"startup unknown", "ab0r7523", func(d *session.InstanceData) { d.StartupStateUnknown = true }},
		{"startup unknown, agent proven gone", "ab0r7524", func(d *session.InstanceData) { d.StartupStateUnknown, d.LostAgentGone = true, true }},
		{"user killed", "ab0r7525", func(d *session.InstanceData) { d.UserKilled = true }},
		{"user killed, agent proven gone", "ab0r7526", func(d *session.InstanceData) { d.UserKilled, d.LostAgentGone = true, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*creates)
			lostFire(t, tc.taskID, repoPath, intendedAt, tc.shape)
			if err := runTaskDelivery(tc.taskID, task.ProjectExpectation{}, replayMarker(tc.taskID, intendedAt)); err != nil {
				t.Fatalf("replay runTaskDelivery: %v", err)
			}
			if got := len(*creates) - before; got != 0 {
				t.Fatalf("a retained-failure row is consumed, never restarted underneath; creates = %d", got)
			}
			if got := lastRunStatus(t, tc.taskID); got != taskStatusAbortedShutdown {
				t.Fatalf("LastRunStatus = %q, want %q", got, taskStatusAbortedShutdown)
			}
			if markerExists(t, tc.taskID) {
				t.Fatal("adoption discharges the marker")
			}
		})
	}
}

// A Lost row whose run already finished was prompted and processed the prompt;
// it is adopted as before, whatever its Lost says.
func TestReconcilePendingTaskFire_AdoptsALostRowWhoseRunFinished(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		taskID string
		gone   bool
	}{
		{"agent may be running", "ab0r7527", false},
		{"agent proven gone", "ab0r7528", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*creates)
			lostFire(t, tc.taskID, repoPath, intendedAt, func(d *session.InstanceData) {
				d.TaskRunActive, d.LostAgentGone = false, tc.gone
			})
			if err := runTaskDelivery(tc.taskID, task.ProjectExpectation{}, replayMarker(tc.taskID, intendedAt)); err != nil {
				t.Fatalf("replay runTaskDelivery: %v", err)
			}
			if got := len(*creates) - before; got != 0 {
				t.Fatalf("a finished run already satisfied the occurrence; creates = %d", got)
			}
			if got := lastRunStatus(t, tc.taskID); got != "started" {
				t.Fatalf("LastRunStatus = %q, want started (adopted)", got)
			}
			if markerExists(t, tc.taskID) {
				t.Fatal("adoption discharges the marker")
			}
		})
	}
}

// A held row does not hide another matched row that is the fire's settled
// product: an earlier replay that already delivered is adopted.
func TestReconcilePendingTaskFire_AnAdoptableRowWinsOverAHeldOne(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	intendedAt := time.Now().Add(-time.Minute)
	repoID := lostFire(t, "ab0r7529", repoPath, intendedAt, nil)
	if err := appendInstanceData(repoID, session.InstanceData{
		ID: "inst-ab0r7529-delivered", TaskID: "ab0r7529", Title: "nightly-delivered", Path: repoPath, Program: "claude",
		Liveness: session.LiveRunning, TaskRunActive: true,
		CreatedAt: intendedAt.Add(time.Minute), UpdatedAt: intendedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("seed delivered row: %v", err)
	}
	fire, err := readTaskFireMarker("ab0r7529")
	if err != nil || fire == nil {
		t.Fatalf("readTaskFireMarker: %v", err)
	}

	done, err := reconcilePendingTaskFire(lastRun(t, "ab0r7529"), fire)
	if err != nil || !done {
		t.Fatalf("the delivered row is the fire's product and must be adopted; done=%v err=%v", done, err)
	}
	if got := lastRunStatus(t, "ab0r7529"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started", got)
	}
}

// (d) A held fire resolves once its row's restore settles: a rebind leaves a
// live row to adopt, and a respawn stamps TaskRunInterrupted, which delivers
// fresh exactly once.
func TestRunTask_HeldReplayResolvesWhenTheRestoreSettles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name        string
		taskID      string
		settle      func(*session.InstanceData)
		wantCreates int
	}{
		{"rebind adopts", "ab0r752a", rebound, 0},
		{"respawn delivers fresh", "ab0r752b", respawned, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*creates)
			repoID := lostFire(t, tc.taskID, repoPath, intendedAt, nil)
			requireHeld(t, runTaskDelivery(tc.taskID, task.ProjectExpectation{}, replayMarker(tc.taskID, intendedAt)))

			settleRow(t, repoID, tc.taskID, tc.settle)
			if err := runTaskDelivery(tc.taskID, task.ProjectExpectation{}, replayMarker(tc.taskID, intendedAt)); err != nil {
				t.Fatalf("the settled look must resolve the fire: %v", err)
			}
			if got := len(*creates) - before; got != tc.wantCreates {
				t.Fatalf("creates = %d, want %d", got, tc.wantCreates)
			}
			if got := lastRunStatus(t, tc.taskID); got != "started" {
				t.Fatalf("LastRunStatus = %q, want started", got)
			}
			if markerExists(t, tc.taskID) {
				t.Fatal("a resolved fire discharges its marker")
			}
		})
	}
}

// The boot replay looks again on its own while the fire is held, and stops once
// the restore settles. The delay seam stands in for the restore loop's backoff
// and settles the row during the first wait.
func TestReplayTaskFire_LooksAgainUntilTheRestoreSettles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name        string
		taskID      string
		settle      func(*session.InstanceData)
		wantCreates int
	}{
		{"rebind adopts", "ab0r752c", rebound, 0},
		{"respawn delivers fresh", "ab0r752d", respawned, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*creates)
			repoID := lostFire(t, tc.taskID, repoPath, intendedAt, nil)
			var waits []int
			orig := taskFireRecheckDelay
			taskFireRecheckDelay = func(attempt int) time.Duration {
				waits = append(waits, attempt)
				if attempt == 1 {
					settleRow(t, repoID, tc.taskID, tc.settle)
				}
				return 0
			}
			t.Cleanup(func() { taskFireRecheckDelay = orig })

			replayTaskFire(replayMarker(tc.taskID, intendedAt))
			if len(waits) != 1 {
				t.Fatalf("the replay must wait once while held and stop once settled; waited at attempts %v", waits)
			}
			if got := len(*creates) - before; got != tc.wantCreates {
				t.Fatalf("creates = %d, want %d", got, tc.wantCreates)
			}
			if markerExists(t, tc.taskID) {
				t.Fatal("a resolved fire discharges its marker")
			}
		})
	}
}

// A stop ends the wait at once and leaves the held fire for the next daemon's
// replay.
func TestReplayTaskFire_StopLeavesAHeldFireForTheNextDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	lostFire(t, "ab0r752e", repoPath, intendedAt, nil)
	pinDaemonDraining(t, true)
	orig := taskFireRecheckDelay
	taskFireRecheckDelay = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { taskFireRecheckDelay = orig })

	done := make(chan struct{})
	go func() { replayTaskFire(replayMarker("ab0r752e", intendedAt)); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stopping daemon must end the held replay's wait at once")
	}
	if len(*creates) != 0 || !markerExists(t, "ab0r752e") {
		t.Fatalf("a stop delivers nothing and keeps the marker for the next daemon; creates = %d", len(*creates))
	}
}

// A held fire never stops the task firing. The next scheduled or manual fire
// delivers its own occurrence and retires the held marker, and a replay of the
// held occurrence then has nothing to deliver.
func TestRunTask_NewFireSupersedesAHeldOne(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	lostFire(t, "ab0r752f", repoPath, intendedAt, nil)

	if err := RunTask("ab0r752f", task.ProjectExpectation{}); err != nil {
		t.Fatalf("a new fire must not wait on a held one: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("the new fire delivers its own occurrence exactly once; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r752f"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started", got)
	}
	if markerExists(t, "ab0r752f") {
		t.Fatal("the new fire's own delivery discharges its marker")
	}

	if err := runTaskDelivery("ab0r752f", task.ProjectExpectation{}, replayMarker("ab0r752f", intendedAt)); err != nil {
		t.Fatalf("replay of the superseded occurrence: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("nothing may deliver the superseded occurrence later as well; creates = %d", len(*creates))
	}
}

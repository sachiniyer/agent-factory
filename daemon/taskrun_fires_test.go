package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// pinDaemonDraining makes the shutdown classifier report draining/not-draining
// for the test's duration.
func pinDaemonDraining(t *testing.T, draining bool) {
	t.Helper()
	orig := daemonDrainingFn
	daemonDrainingFn = func() bool { return draining }
	t.Cleanup(func() { daemonDrainingFn = orig })
}

// fireMarkerPath returns the marker file's path inside the test's AF home.
func fireMarkerPath(t *testing.T, taskID string) string {
	t.Helper()
	dir, err := taskFireDir()
	if err != nil {
		t.Fatalf("taskFireDir: %v", err)
	}
	return filepath.Join(dir, taskID+".json")
}

func seedCronTask(t *testing.T, id, repoPath string) {
	t.Helper()
	if err := task.AddTask(task.Task{
		ID:          id,
		Name:        "nightly",
		Prompt:      "do it",
		CronExpr:    "0 3 * * *",
		ProjectPath: repoPath,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

func seedPendingFire(t *testing.T, m taskFireMarker) {
	t.Helper()
	marker := m
	if err := writeTaskFireMarker(&marker); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

// replayMarker is the snapshot a boot scan hands runTaskDelivery: identity is
// task_id + intended_at, so the replay only ever satisfies that occurrence.
func replayMarker(taskID string, intendedAt time.Time) *taskFireMarker {
	return &taskFireMarker{TaskID: taskID, IntendedAt: intendedAt}
}

func markerExists(t *testing.T, taskID string) bool {
	t.Helper()
	m, err := readTaskFireMarker(taskID)
	if err != nil {
		t.Fatalf("readTaskFireMarker: %v", err)
	}
	return m != nil
}

func lastRunStatus(t *testing.T, taskID string) string {
	t.Helper()
	got, err := task.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return got.LastRunStatus
}

// TestTaskFireMarkerRoundTrip covers the ledger itself: ensure writes once and
// reports subsequent reads as pending, the create-path binds land, and clear
// discharges it.
func TestTaskFireMarkerRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	fire, pending, err := ensureTaskFireMarker("a1b2c3d4")
	if err != nil {
		t.Fatalf("ensureTaskFireMarker: %v", err)
	}
	if pending || fire.IntendedAt.IsZero() {
		t.Fatalf("first ensure: pending=%v intendedAt=%v, want fresh marker", pending, fire.IntendedAt)
	}

	noteTaskFireTitle("a1b2c3d4", "nightly-2")
	noteTaskFireInstance("a1b2c3d4", "inst-xyz")

	fire, pending, err = ensureTaskFireMarker("a1b2c3d4")
	if err != nil {
		t.Fatalf("second ensureTaskFireMarker: %v", err)
	}
	if !pending {
		t.Fatal("second ensure must report the existing marker as pending")
	}
	if fire.Title != "nightly-2" || fire.InstanceID != "inst-xyz" {
		t.Fatalf("binds did not persist: %+v", fire)
	}

	clearTaskFire("a1b2c3d4")
	if markerExists(t, "a1b2c3d4") {
		t.Fatal("clearTaskFire did not remove the marker")
	}

	// Binds on a task with no marker must be no-ops — only RunTask writes one.
	noteTaskFireTitle("ffffffff", "nope")
	if markerExists(t, "ffffffff") {
		t.Fatal("a bind must never create a marker")
	}
}

// TestIsShutdownAbortErr covers the shutdown-shaped error set the deferred
// writer classifies on a draining daemon.
func TestIsShutdownAbortErr(t *testing.T) {
	for name, err := range map[string]error{
		"unexpected EOF":      fmt.Errorf("failed to start task session: %w", io.ErrUnexpectedEOF),
		"plain EOF":           io.EOF,
		"quiescing refusal":   errDaemonQuiescing(),
		"context canceled":    fmt.Errorf("deliver: %w", context.Canceled),
		"admission deadline":  daemonAdmissionDeadlineError(),
		"absent socket (nil)": nil,
	} {
		if name == "absent socket (nil)" {
			if isShutdownAbortErr(err) {
				t.Fatal("nil error must not classify as shutdown-shaped")
			}
			continue
		}
		if !isShutdownAbortErr(err) {
			t.Fatalf("%s should classify as shutdown-shaped", name)
		}
	}
	if isShutdownAbortErr(errors.New("boom: not a transport error")) {
		t.Fatal("ordinary error must not classify as shutdown-shaped")
	}
}

// TestRunTask_ShutdownAbortClassifiesAndKeepsMarker is the #5503 regression at
// its smallest scale: a delivery dying with unexpected EOF while the daemon
// drains must record "aborted: daemon shutting down" and KEEP the marker, not
// "errored: ... unexpected EOF" with the occurrence consumed.
func TestRunTask_ShutdownAbortClassifiesAndKeepsMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7001", repo)
	pinDaemonDraining(t, true)

	origCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, io.ErrUnexpectedEOF
	}
	t.Cleanup(func() { createSessionForTask = origCreate })

	if err := RunTask("ab0r7001", task.ProjectExpectation{}); err == nil {
		t.Fatal("expected the delivery failure to surface")
	}
	if got := lastRunStatus(t, "ab0r7001"); got != taskStatusAbortedShutdown {
		t.Fatalf("LastRunStatus = %q, want %q", got, taskStatusAbortedShutdown)
	}
	if !markerExists(t, "ab0r7001") {
		t.Fatal("an aborted fire must keep its marker for the replacement daemon")
	}
}

// TestRunTask_OrdinaryErrorClearsMarker pins the other half: a delivery failure
// on a non-draining daemon is an ordinary task error — "errored:" status, and
// the marker is discharged because the occurrence concluded.
func TestRunTask_OrdinaryErrorClearsMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7002", repo)

	origCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, io.ErrUnexpectedEOF // same wire shape, but NOT during a drain
	}
	t.Cleanup(func() { createSessionForTask = origCreate })

	if err := RunTask("ab0r7002", task.ProjectExpectation{}); err == nil {
		t.Fatal("expected the delivery failure to surface")
	}
	if got := lastRunStatus(t, "ab0r7002"); !strings.HasPrefix(got, "errored:") {
		t.Fatalf("LastRunStatus = %q, want errored:", got)
	}
	if markerExists(t, "ab0r7002") {
		t.Fatal("an errored fire must discharge its marker")
	}
}

// TestRunTask_SuccessClearsMarker covers the terminal path: a delivered run
// discharges its marker so a later boot cannot replay a completed occurrence.
func TestRunTask_SuccessClearsMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7003", repo)
	creates, _ := stubTaskDelivery(t)

	if err := RunTask("ab0r7003", task.ProjectExpectation{}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("creates = %d, want 1", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7003"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started", got)
	}
	if markerExists(t, "ab0r7003") {
		t.Fatal("a delivered fire must discharge its marker")
	}
}

// TestRunTask_ReplayDeliversOwedFire is the catch-up half of #5503: a pending
// marker with no residue and no persisted row means the occurrence is still
// owed — the replay path must deliver it exactly once.
func TestRunTask_ReplayDeliversOwedFire(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7004", repo)
	creates, _ := stubTaskDelivery(t)
	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7004",
		IntendedAt: intendedAt,
		Title:      "nightly-2",
		InstanceID: "inst-abc",
	})

	if err := runTaskDelivery("ab0r7004", task.ProjectExpectation{}, replayMarker("ab0r7004", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("the owed fire must be delivered once; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7004"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started", got)
	}
	if markerExists(t, "ab0r7004") {
		t.Fatal("a replayed fire must discharge its marker on delivery")
	}
}

// TestRunTask_ReplayAdoptsPersistedRow: the interrupted create already persisted
// its row — the replay must adopt it rather than start a second session.
func TestRunTask_ReplayAdoptsPersistedRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7005", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:        "inst-fire5",
		TaskID:    "ab0r7005",
		Title:     "nightly-2",
		Path:      repoPath,
		Program:   "claude",
		Status:    session.Running,
		Liveness:  session.LiveReady,
		CreatedAt: intendedAt.Add(2 * time.Second),
		UpdatedAt: intendedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7005",
		IntendedAt: intendedAt,
		Title:      "nightly-2",
		InstanceID: "inst-fire5",
	})

	if err := runTaskDelivery("ab0r7005", task.ProjectExpectation{}, replayMarker("ab0r7005", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("an adopted row must not be re-created; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7005"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started", got)
	}
	if markerExists(t, "ab0r7005") {
		t.Fatal("adoption must discharge the marker")
	}
}

// TestRunTask_ReplayAdoptedTombstoneIsAborted: a row the aborted create left
// tombstoned is already owned by the teardown machinery — the fire records
// aborted, and nothing is delivered underneath it.
func TestRunTask_ReplayAdoptedTombstoneIsAborted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7006", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:         "inst-fire6",
		TaskID:     "ab0r7006",
		Title:      "nightly-3",
		Path:       repoPath,
		Program:    "claude",
		Status:     session.Loading,
		Liveness:   session.LiveReady,
		UserKilled: true,
		CreatedAt:  intendedAt.Add(2 * time.Second),
		UpdatedAt:  intendedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7006",
		IntendedAt: intendedAt,
		Title:      "nightly-3",
		InstanceID: "inst-fire6",
	})

	if err := runTaskDelivery("ab0r7006", task.ProjectExpectation{}, replayMarker("ab0r7006", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("an adopted tombstone must not be re-created; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7006"); got != taskStatusAbortedShutdown {
		t.Fatalf("LastRunStatus = %q, want %q", got, taskStatusAbortedShutdown)
	}
	if markerExists(t, "ab0r7006") {
		t.Fatal("adoption must discharge the marker")
	}
}

// TestRunTask_ReplayResidueDischarges: the marker survived but the run's status
// already landed — the replay consumes it without delivering.
func TestRunTask_ReplayResidueDischarges(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7007", repo)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	finished := time.Now()
	if _, err := task.UpdateTaskStatus("ab0r7007", &finished, "started"); err != nil {
		t.Fatalf("seed run status: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7007", IntendedAt: intendedAt})

	if err := runTaskDelivery("ab0r7007", task.ProjectExpectation{}, replayMarker("ab0r7007", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("a completed fire must not re-deliver; creates = %d", len(*creates))
	}
	if markerExists(t, "ab0r7007") {
		t.Fatal("residue must discharge the marker")
	}
}

// TestRunTask_FreshFireOnStaleMarker: a NEW cron fire landing on a stale marker
// still owes its own occurrence — the stale marker is discharged, a fresh one
// is written, and the run delivers normally.
func TestRunTask_FreshFireOnStaleMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7008", repo)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Hour)
	finished := time.Now().Add(-30 * time.Minute)
	if _, err := task.UpdateTaskStatus("ab0r7008", &finished, "started"); err != nil {
		t.Fatalf("seed run status: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7008", IntendedAt: intendedAt})

	if err := runTaskDelivery("ab0r7008", task.ProjectExpectation{}, nil); err != nil {
		t.Fatalf("fresh runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("a fresh fire must still deliver; creates = %d", len(*creates))
	}
	if markerExists(t, "ab0r7008") {
		t.Fatal("the discharged-then-delivered fire must not leave a marker")
	}
}

// TestRunTask_AbortedStatusIsNotResidue: an aborted run's own status must not
// satisfy the residue check — the marker's intent is still owed.
func TestRunTask_AbortedStatusIsNotResidue(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7009", repo)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	abortedAt := intendedAt.Add(30 * time.Second)
	if _, err := task.UpdateTaskStatus("ab0r7009", &abortedAt, taskStatusAbortedShutdown); err != nil {
		t.Fatalf("seed run status: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7009", IntendedAt: intendedAt, Title: "nightly-9"})

	if err := runTaskDelivery("ab0r7009", task.ProjectExpectation{}, replayMarker("ab0r7009", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("an aborted fire must be delivered by replay; creates = %d", len(*creates))
	}
}

// TestRunTask_ForeignRowOnBoundTitle: a row holding the bound title that is NOT
// this attempt's (different task association, older create) makes the leftover
// foreign — reconcile leaves it alone and the fire delivers fresh.
func TestRunTask_ForeignRowOnBoundTitle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7010", repoPath)
	creates, _ := stubTaskDelivery(t)

	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:        "inst-foreign",
		TaskID:    "s0me0ther",
		Title:     "nightly-2",
		Path:      repoPath,
		Program:   "claude",
		Status:    session.Running,
		Liveness:  session.LiveReady,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed foreign row: %v", err)
	}
	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7010",
		IntendedAt: intendedAt,
		Title:      "nightly-2",
	})

	if err := runTaskDelivery("ab0r7010", task.ProjectExpectation{}, replayMarker("ab0r7010", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("the fire is still owed and must deliver fresh; creates = %d", len(*creates))
	}
	// The foreign row must be untouched.
	rows, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "inst-foreign" {
		t.Fatalf("foreign row was disturbed: %+v", rows)
	}
}

// TestReplayPendingTaskFires covers the boot scan: enabled tasks re-enter
// RunTask; gone, disabled, and watch markers are discharged without firing.
func TestReplayPendingTaskFires(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	creates, _ := stubTaskDelivery(t)

	seedCronTask(t, "ab0r7011", repo)
	if err := task.AddTask(task.Task{
		ID: "ab0r7012", Name: "off", CronExpr: "0 3 * * *",
		ProjectPath: repo, Enabled: false, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed disabled task: %v", err)
	}
	if err := task.AddTask(task.Task{
		ID: "ab0r7013", Name: "watcher", WatchCmd: "tail -f x",
		ProjectPath: repo, Enabled: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed watch task: %v", err)
	}
	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7011", IntendedAt: intendedAt})
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7012", IntendedAt: intendedAt})
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7013", IntendedAt: intendedAt})
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7999", IntendedAt: intendedAt}) // no such task

	replayPendingTaskFires()

	if len(*creates) != 1 || (*creates)[0].TaskID != "ab0r7011" {
		t.Fatalf("only the enabled cron task's fire must replay; creates = %+v", *creates)
	}
	for _, id := range []string{"ab0r7011", "ab0r7012", "ab0r7013", "ab0r7999"} {
		if markerExists(t, id) {
			t.Fatalf("marker for %s must be discharged by the scan", id)
		}
	}
}

// TestRunTask_ReplayConsumedMarkerSkips: a racing fire that already discharged
// the pending marker owns the occurrence — a late-arriving replay of the same
// snapshot must not mint a fresh marker and deliver a duplicate.
func TestRunTask_ReplayConsumedMarkerSkips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7014", repo)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7014", IntendedAt: intendedAt})
	clearTaskFire("ab0r7014")

	if err := runTaskDelivery("ab0r7014", task.ProjectExpectation{}, replayMarker("ab0r7014", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("a consumed marker must not deliver; creates = %d", len(*creates))
	}
	if markerExists(t, "ab0r7014") {
		t.Fatal("a consumed-marker replay must not mint a fresh marker")
	}
}

// TestRunTask_ReplaySupersededMarkerSkips: the on-disk marker names a NEWER
// occurrence than the snapshot the boot scan read — the replay must leave it
// for its own run, not consume it.
func TestRunTask_ReplaySupersededMarkerSkips(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7015", repo)
	creates, _ := stubTaskDelivery(t)

	staleAt := time.Now().Add(-time.Hour)
	freshAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7015", IntendedAt: freshAt})

	if err := runTaskDelivery("ab0r7015", task.ProjectExpectation{}, replayMarker("ab0r7015", staleAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 0 {
		t.Fatalf("a superseded snapshot must not deliver; creates = %d", len(*creates))
	}
	m, err := readTaskFireMarker("ab0r7015")
	if err != nil || m == nil || !m.IntendedAt.Equal(freshAt) {
		t.Fatalf("the newer occurrence's marker must survive untouched; marker = %+v, err = %v", m, err)
	}
}

// TestRunTask_ReconcileErrorIsNotResidue: a failed reconcile records an
// "errored: task fire reconcile" status with a fresh timestamp — the next boot
// must still retry the owed fire rather than read that failure as completion.
func TestRunTask_ReconcileErrorIsNotResidue(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7016", repo)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	failedAt := intendedAt.Add(30 * time.Second)
	status := "errored: " + fmt.Errorf("%w: x", errTaskFireReconcile).Error()
	if _, err := task.UpdateTaskStatus("ab0r7016", &failedAt, status); err != nil {
		t.Fatalf("seed run status: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7016", IntendedAt: intendedAt})

	if err := runTaskDelivery("ab0r7016", task.ProjectExpectation{}, replayMarker("ab0r7016", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("a reconcile failure is not an outcome; the owed fire must deliver, creates = %d", len(*creates))
	}
}

func seedTargetedCronTask(t *testing.T, id, repoPath, target string) {
	t.Helper()
	if err := task.AddTask(task.Task{
		ID:            id,
		Name:          "targeted",
		Prompt:        "do it",
		CronExpr:      "0 3 * * *",
		ProjectPath:   repoPath,
		TargetSession: target,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// TestRunTask_TargetedFireSendAttribution pins the shared-target rule (#5503
// review): a send landing on the SAME target after the marker's intent only
// discharges the fire when the attempt is positively attributed to this task.
// Another task's send, or an interactive (unattributed) send, cannot prove
// this fire's delivery ever happened — the occurrence stays owed.
func TestRunTask_TargetedFireSendAttribution(t *testing.T) {
	for name, tc := range map[string]struct {
		attribution  string
		wantDelivers int
		wantMarker   bool
	}{
		"own send discharges":     {attribution: "task", wantDelivers: 0, wantMarker: false},
		"another task stays owed": {attribution: "ot4r9999", wantDelivers: 1, wantMarker: false},
		"unattributed stays owed": {attribution: "", wantDelivers: 1, wantMarker: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			repoPath := setupTaskRepo(t)
			seedTargetedCronTask(t, "ab0r7020", repoPath, "shared-target")
			_, delivers := stubTaskDelivery(t)

			intendedAt := time.Now().Add(-time.Minute)
			repo, err := config.RepoFromPath(repoPath)
			if err != nil {
				t.Fatalf("RepoFromPath: %v", err)
			}
			attribution := tc.attribution
			if attribution == "task" {
				attribution = "ab0r7020"
			}
			if err := appendInstanceData(repo.ID, session.InstanceData{
				ID:        "inst-target",
				Title:     "shared-target",
				Path:      repoPath,
				Program:   "claude",
				Status:    session.Running,
				Liveness:  session.LiveReady,
				CreatedAt: intendedAt.Add(-time.Hour),
				UpdatedAt: intendedAt.Add(-time.Hour),
				// A send landed on the shared target after the intent — but
				// attribution is what decides whose send it was.
				LastPromptAttemptAt:      intendedAt.Add(30 * time.Second),
				LastPromptDeliveryStatus: session.PromptDelivered,
				LastPromptTaskID:         attribution,
			}); err != nil {
				t.Fatalf("seed target row: %v", err)
			}
			seedPendingFire(t, taskFireMarker{TaskID: "ab0r7020", IntendedAt: intendedAt})

			if err := runTaskDelivery("ab0r7020", task.ProjectExpectation{}, replayMarker("ab0r7020", intendedAt)); err != nil {
				t.Fatalf("replay runTaskDelivery: %v", err)
			}
			if len(*delivers) != tc.wantDelivers {
				t.Fatalf("delivers = %d, want %d", len(*delivers), tc.wantDelivers)
			}
			if tc.wantDelivers == 1 && (*delivers)[0].TaskID != "ab0r7020" {
				t.Fatalf("the replayed send must carry this fire's task id; TaskID = %q", (*delivers)[0].TaskID)
			}
			if got := markerExists(t, "ab0r7020"); got != tc.wantMarker {
				t.Fatalf("marker exists = %v, want %v", got, tc.wantMarker)
			}
		})
	}
}

// TestRunTask_FreshFireAfterAdoptionDelivers pins adopt-then-deliver (#5503
// review): an interrupted fire whose create persisted is ADOPTED — that
// discharges the OLD occurrence only. A fresh trigger reaching the marker
// first is a NEW occurrence and still owes its own delivery: two occurrences,
// two sessions.
func TestRunTask_FreshFireAfterAdoptionDelivers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7040", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	// The interrupted fire's product: a task-associated row created after the
	// intent — reconcile adopts it as occurrence one's delivery.
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:        "inst-old",
		Title:     "adopted-old",
		Path:      repoPath,
		Program:   "claude",
		Status:    session.Running,
		Liveness:  session.LiveReady,
		TaskID:    "ab0r7040",
		CreatedAt: intendedAt.Add(time.Second),
		UpdatedAt: intendedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("seed adopted row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7040", IntendedAt: intendedAt})

	// A FRESH invocation (no replay marker): the adoption discharges the old
	// occurrence, then this occurrence must mint its own marker and deliver.
	if err := runTaskDelivery("ab0r7040", task.ProjectExpectation{}, nil); err != nil {
		t.Fatalf("fresh runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("adoption discharges the old occurrence only — the fresh fire still owes a delivery; creates = %d", len(*creates))
	}
	if markerExists(t, "ab0r7040") {
		t.Fatalf("the fresh fire's marker must clear once its delivery lands")
	}
}

// TestRunTask_FreshFireAfterTargetedSendDelivers is the same rule on the
// targeted path: a send attributed to this task at-or-after the intent is the
// OLD occurrence's delivery; the fresh trigger owes its own send — two
// occurrences, two sends.
func TestRunTask_FreshFireAfterTargetedSendDelivers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedTargetedCronTask(t, "ab0r7041", repoPath, "shared-target")
	_, delivers := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                       "inst-target",
		Title:                    "shared-target",
		Path:                     repoPath,
		Program:                  "claude",
		Status:                   session.Running,
		Liveness:                 session.LiveReady,
		CreatedAt:                intendedAt.Add(-time.Hour),
		UpdatedAt:                intendedAt.Add(-time.Hour),
		LastPromptAttemptAt:      intendedAt.Add(30 * time.Second),
		LastPromptDeliveryStatus: session.PromptDelivered,
		LastPromptTaskID:         "ab0r7041",
	}); err != nil {
		t.Fatalf("seed target row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7041", IntendedAt: intendedAt})

	if err := runTaskDelivery("ab0r7041", task.ProjectExpectation{}, nil); err != nil {
		t.Fatalf("fresh runTaskDelivery: %v", err)
	}
	if len(*delivers) != 1 {
		t.Fatalf("the attributed send discharged the old occurrence only — the fresh fire still owes its own send; delivers = %d", len(*delivers))
	}
	if (*delivers)[0].TaskID != "ab0r7041" {
		t.Fatalf("the fresh send must carry this fire's task id; TaskID = %q", (*delivers)[0].TaskID)
	}
	if markerExists(t, "ab0r7041") {
		t.Fatalf("the fresh fire's marker must clear once its send lands")
	}
}

// TestRunTask_ReconcileRejectsRepointedRepo pins the retained-binding check
// (#5503 review): a task whose project path no longer resolves its retained
// RepoID must never reconcile that repository's rows — a matching leftover
// identity there belongs to someone else. The reconcile refuses, the marker
// stays owed, and nothing is delivered or mutated.
func TestRunTask_ReconcileRejectsRepointedRepo(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7050", repoPath)
	creates, _ := stubTaskDelivery(t)

	// Repoint the retained binding on disk: the project path still resolves a
	// real repo, just not the one this task was bound to.
	tasksPath := filepath.Join(home, "tasks.json")
	raw, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("read tasks.json: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("parse tasks.json: %v", err)
	}
	tasks, ok := env["tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("expected one task row, got %v", env["tasks"])
	}
	tasks[0].(map[string]any)["repo_id"] = "repo-other"
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal tasks.json: %v", err)
	}
	if err := os.WriteFile(tasksPath, out, 0o600); err != nil {
		t.Fatalf("rewrite tasks.json: %v", err)
	}

	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7050", IntendedAt: intendedAt, Title: "repointed"})

	err = runTaskDelivery("ab0r7050", task.ProjectExpectation{}, nil)
	if !errors.Is(err, errTaskFireReconcile) {
		t.Fatalf("a repointed binding must refuse reconcile, got err=%v", err)
	}
	if !markerExists(t, "ab0r7050") {
		t.Fatalf("a refused reconcile keeps the fire owed — marker was discharged")
	}
	if len(*creates) != 0 {
		t.Fatalf("nothing may deliver while the binding is unresolved; creates = %d", len(*creates))
	}
}

// TestTaskFireLeftoverRecordCarriesTmuxName: the quarantine/tombstone record's
// recovery handle must name the exact tmux session it stands for (#5503
// review). restoreLocalTabs reads TmuxName — leaving it empty rebuilds an
// unscoped name that would teardown a different session. An intent in the
// future makes every per-attempt evidence check fail, so the record takes the
// quarantine shape on any host, with or without a tmux server.
func TestTaskFireLeftoverRecordCarriesTmuxName(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	title := "leftover-title"
	branch := git.BranchForTitle(cfg.BranchPrefix, title)
	wtPath := filepath.Join(t.TempDir(), "leftover-wt")
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("seed leftover worktree: %v: %s", err, out)
	}

	fire := &taskFireMarker{
		TaskID:     "ab0r7060",
		IntendedAt: time.Now().Add(time.Hour), // future: nothing can postdate it — unproven
		Title:      title,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7060", ProjectPath: repoPath, Prompt: "p"},
		fire, repo, nil)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil {
		t.Fatalf("expected a quarantine record for the unproven leftover worktree")
	}
	if want := tmux.SanitizedNameForRepo(title, repo.IdentityPath()); row.TmuxName != want {
		t.Fatalf("record's recovery handle names %q, want the exact leftover tmux name %q", row.TmuxName, want)
	}
	if !row.StartupStateUnknown {
		t.Fatalf("an unproven leftover must quarantine as startup-state-unknown")
	}
}

// TestReplayPendingTaskFires_TaskReadErrorKeepsMarker: a store read or parse
// failure is not "the task was deleted" — the marker must survive so a later
// boot, once the store reads cleanly, can still satisfy the owed intent. Only
// a positively-established not-found discharges it.
func TestReplayPendingTaskFires_TaskReadErrorKeepsMarker(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7030", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7030", IntendedAt: intendedAt})
	// A second marker names a task that provably does not exist.
	seedPendingFire(t, taskFireMarker{TaskID: "ab0r7031", IntendedAt: intendedAt})

	tasksPath := filepath.Join(home, "tasks.json")
	orig, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("read tasks.json: %v", err)
	}
	if err := os.WriteFile(tasksPath, []byte(`{"tasks": [`), 0o600); err != nil {
		t.Fatalf("corrupt tasks.json: %v", err)
	}

	replayPendingTaskFires()

	// Neither marker may be discharged: the store never answered, so neither
	// task's existence was positively established either way.
	for _, id := range []string{"ab0r7030", "ab0r7031"} {
		if !markerExists(t, id) {
			t.Fatalf("marker %s was discharged on a read error — intent lost", id)
		}
	}
	if len(*creates) != 0 {
		t.Fatalf("no delivery may run while the store is unreadable; creates = %d", len(*creates))
	}

	// Once the store reads cleanly again the next boot settles both markers:
	// the live task replays, the deleted one discharges.
	if err := os.WriteFile(tasksPath, orig, 0o600); err != nil {
		t.Fatalf("restore tasks.json: %v", err)
	}
	replayPendingTaskFires()

	if len(*creates) != 1 {
		t.Fatalf("the live task's owed fire must replay once the store heals; creates = %d", len(*creates))
	}
	for _, id := range []string{"ab0r7030", "ab0r7031"} {
		if markerExists(t, id) {
			t.Fatalf("marker %s survived a clean reconcile — replay must settle it", id)
		}
	}
}

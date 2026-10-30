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

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
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

// TestRunTask_ReplayDoesNotAdoptAnInterruptedRow is the #4222 crash window at
// the fire-marker axis: a create committed its row and bound the marker, the
// daemon died before the outcome cleared, and on restart the row's runtime had
// to be respawned — a replacement that was never replayed the prompt. The
// pending marker is still owed its delivery; reconcile must not adopt the
// interrupted row as the fire's product and misreport the undelivered
// occurrence "started".
func TestRunTask_ReplayDoesNotAdoptAnInterruptedRow(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)
	seedCronTask(t, "ab0r7100", repoPath)
	creates, _ := stubTaskDelivery(t)

	intendedAt := time.Now().Add(-time.Minute)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	if err := appendInstanceData(repo.ID, session.InstanceData{
		ID:                 "inst-int1",
		TaskID:             "ab0r7100",
		Title:              "nightly-4",
		Path:               repoPath,
		Program:            "claude",
		Status:             session.Ready,
		Liveness:           session.LiveReady,
		TaskRunInterrupted: true,
		CreatedAt:          intendedAt.Add(2 * time.Second),
		UpdatedAt:          intendedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("seed session row: %v", err)
	}
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7100",
		IntendedAt: intendedAt,
		Title:      "nightly-4",
		InstanceID: "inst-int1",
	})

	if err := runTaskDelivery("ab0r7100", task.ProjectExpectation{}, replayMarker("ab0r7100", intendedAt)); err != nil {
		t.Fatalf("replay runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("an interrupted row is not the fire's product — the still-owed occurrence must deliver fresh; creates = %d", len(*creates))
	}
	if got := lastRunStatus(t, "ab0r7100"); got != "started" {
		t.Fatalf("LastRunStatus = %q, want started (the fresh delivery's own run)", got)
	}
	if markerExists(t, "ab0r7100") {
		t.Fatal("the fresh delivery discharges the marker")
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
		fire, repo, nil, time.Now())
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

// TestTaskFireResidue_RejectsArmingVerdicts pins the arming-vs-run distinction:
// armTaskAutomation can rewrite an interrupted run's "aborted:" status to a
// not-armed refusal (or clearStaleNotArmedStatus's empty one) while preserving
// LastRunAt. Neither is evidence a run completed — accepting them would
// discharge a fire that never delivered.
func TestTaskFireResidue_RejectsArmingVerdicts(t *testing.T) {
	fire := &taskFireMarker{TaskID: "x", IntendedAt: time.Now().Add(-time.Minute)}
	at := time.Now()
	for _, tc := range []struct {
		name   string
		status string
		want   bool
	}{
		{"arming-cleared empty status", "", false},
		{"arming refusal", notArmedPrefix + "root agent policy disabled", false},
		{"shutdown abort", taskStatusAbortedShutdown, false},
		{"other abort", "aborted: superseded", false},
		{"reconcile failure", "errored: " + errTaskFireReconcile.Error(), false},
		{"real run error", "errored: unexpected EOF", true},
		{"sent", "sent", true},
		{"started", "started", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := taskFireResidue(&task.Task{LastRunAt: &at, LastRunStatus: tc.status}, fire)
			if got != tc.want {
				t.Fatalf("taskFireResidue(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
	// A timestamp before the intent is never residue, whatever the status.
	pre := fire.IntendedAt.Add(-time.Second)
	if taskFireResidue(&task.Task{LastRunAt: &pre, LastRunStatus: "started"}, fire) {
		t.Fatal("a status written before the intent must never discharge it")
	}
}

// TestTaskFireLeftoverRecord_StopsProvenRuntime pins the writer-stop ordering:
// a proven leftover's tmux runtime can still be writing its worktree while the
// cleanliness probe runs, and a clean snapshot taken before the stop would
// authorize a `worktree remove -f` teardown that deletes work generated
// afterwards. The runtime is killed before the probe, so the row persisted
// for a clean leftover is a tombstone AND the session is already dead.
func TestTaskFireLeftoverRecord_StopsProvenRuntime(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	title := "writer-leftover"
	branch := git.BranchForTitle(cfg.BranchPrefix, title)
	sanitized := tmux.SanitizedNameForRepo(title, repo.IdentityPath())
	wtPath := filepath.Join(t.TempDir(), "writer-wt")
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("seed leftover worktree: %v: %s", err, out)
	}
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession+"="+sanitized,
		"-e", tmux.EnvMarkerGeneration+"=gen-proven-leftover",
		"-e", tmux.EnvMarkerHome+"="+home, "sh").CombinedOutput(); err != nil {
		t.Fatalf("seed leftover tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run()
	})

	// Cross a wall-second so the leftover's second-granular creation time
	// strictly predates the reconcile bound: the upper bound now rejects the
	// boot's own wall second (a concurrent create racing the boot lands there),
	// so a genuine leftover must be seeded in an earlier second to read as ours.
	time.Sleep(1100 * time.Millisecond)
	fire := &taskFireMarker{
		TaskID: "ab0r7097", IntendedAt: time.Now().Add(-time.Minute), Title: title,
	}
	// reconcileStart is captured AFTER the leftover session is seeded, so the
	// genuine leftover's creation time predates the bound and reads as ours —
	// the same ordering a boot replay sees (leftover from the old daemon,
	// reconcile bound = this daemon's boot).
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7097", ProjectPath: repoPath, Prompt: "p"},
		fire, repo, nil, time.Now())
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row == nil || !row.UserKilled {
		t.Fatalf("a proven clean leftover must record a teardown tombstone, got %+v", row)
	}
	if row.StartupStateUnknown {
		t.Fatal("a proven clean leftover must not be quarantined")
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err == nil {
		t.Fatalf("the proven leftover's runtime must be stopped inside reconcile, still alive: %s", out)
	}
}

// TestRunTask_UpgradeGateErrorKeepsMarker: a typed upgrade-gate refusal from
// the loopback RPC is self-proving shutdown evidence — a live upgrade
// transaction only exists across a daemon replacement, and it can still be
// open after the OLD daemon's drain flag is gone, so the classification must
// not require daemonDrainingFn. Without it the occurrence would be recorded
// "errored:" and its marker discharged undelivered (#5503 review).
func TestRunTask_UpgradeGateErrorKeepsMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7100", repo)
	pinDaemonDraining(t, false) // deliberately not draining: the gate error alone must classify

	origCreate := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		return nil, &upgradetxn.UpgradeInProgressError{TransactionID: "txn-1", ToVersion: "v2"}
	}
	t.Cleanup(func() { createSessionForTask = origCreate })

	if err := RunTask("ab0r7100", task.ProjectExpectation{}); err == nil {
		t.Fatal("expected the delivery failure to surface")
	}
	if got := lastRunStatus(t, "ab0r7100"); got != taskStatusAbortedShutdown {
		t.Fatalf("LastRunStatus = %q, want %q", got, taskStatusAbortedShutdown)
	}
	if !markerExists(t, "ab0r7100") {
		t.Fatal("a fire blocked by the upgrade gate must keep its marker for the replacement daemon")
	}
}

// TestTaskFireLeftoverRecord_ForeignRowOwningCollidingTmuxName pins the
// foreign-row scan's newly added tmux-identity clause. Titles whose distinct
// spellings the tmux sanitizer collapses onto one name (e.g. "api/refactor"
// and "api_refactor" both map to af_<repoHash>_api_refactor) while branch
// sanitization keeps '/' distinct (so the derived branches differ) MUST be
// treated as foreign when a persisted row owns that tmux name. The reconcile
// reaps, probes, and kills by the sanitized tmux name; before the fix a
// foreign row colliding only on that name passed the ownership scan and the
// leftover was treated as ours to quarantine — and on a host with a live
// session under that name, killed (#5544 review).
//
// This is the no-tmux half of the regression: a seeded leftover worktree makes
// the function discover residue (so it would otherwise quarantine), and a
// foreign row colliding only on the tmux name must force the foreign verdict
// (nil, nil) instead. The end-to-end kill path is covered by the tmux-gated
// sibling TestTaskFireLeftoverRecord_ForeignRowOwningTmuxNameDoesNotReap.
func TestTaskFireLeftoverRecord_ForeignRowOwningCollidingTmuxName(t *testing.T) {
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
	fireTitle := "api/refactor"
	foreignTitle := "api_refactor"
	branch := git.BranchForTitle(cfg.BranchPrefix, fireTitle)
	sanitized := tmux.SanitizedNameForRepo(fireTitle, repo.IdentityPath())

	// The collision is the whole premise of the bug: the two titles share one
	// sanitized tmux name (so the kill path addresses the foreign session),
	// while their derived branches differ (so no branch clause catches the
	// foreign row either).
	if got := tmux.SanitizedNameForRepo(foreignTitle, repo.IdentityPath()); got != sanitized {
		t.Fatalf("collision precondition failed: foreign title sanitizes to %q, want %q", got, sanitized)
	}
	if foreignBranch := git.BranchForTitle(cfg.BranchPrefix, foreignTitle); foreignBranch == branch {
		t.Fatalf("branches must differ so the branch clause cannot catch the foreign row; both = %q", branch)
	}

	// A leftover worktree on the derived branch makes the function discover
	// residue; an intent in the future keeps it unproven so the function would
	// return a quarantine record (never reach the kill) on a host without a
	// colliding live session, isolating the foreign-scan verdict from the
	// runtime-stop ordering.
	wtPath := filepath.Join(t.TempDir(), "leftover-wt")
	if out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branch, wtPath).CombinedOutput(); err != nil {
		t.Fatalf("seed leftover worktree: %v: %s", err, out)
	}
	fire := &taskFireMarker{
		TaskID:     "ab0r7110",
		IntendedAt: time.Now().Add(time.Hour), // future: worktree evidence cannot postdate it → unproven
		Title:      fireTitle,
	}
	foreignRow := session.InstanceData{
		ID:       "inst-foreign-tmux",
		TaskID:   "s0me0ther",
		Title:    foreignTitle,
		Path:     repoPath,
		Branch:   git.BranchForTitle(cfg.BranchPrefix, foreignTitle),
		TmuxName: sanitized, // owns the colliding tmux identity
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7110", ProjectPath: repoPath, Prompt: "p"},
		fire, repo, []session.InstanceData{foreignRow}, time.Now())
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil {
		t.Fatalf("a foreign persisted row owns the colliding tmux name; the leftover must be left alone, got record %+v", row)
	}
}

// TestTaskFireLeftoverRecord_ForeignRowOwningTmuxNameDoesNotReap is the
// end-to-end regression for #5544's tmux-name-collision gap. A persisted
// foreign row whose TmuxName equals the fire's sanitized name — because the
// sanitizer collapses distinct titles like "api/refactor" and "api_refactor"
// to one session name while their branches differ — must be left completely
// alone when the reconcile probes and kills by that name. Before the fix the
// foreign row passed the ownership scan, satisfied the per-attempt evidence
// checks (same AF ancestry markers and a creation time at-or-after the
// intent), and its live tmux session was killed mid-work with no quarantine
// or tombstone record left behind. The session is seeded exactly as a
// normally-created af local session looks.
func TestTaskFireLeftoverRecord_ForeignRowOwningTmuxNameDoesNotReap(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	fireTitle := "api/refactor"
	foreignTitle := "api_refactor"
	sanitized := tmux.SanitizedNameForRepo(fireTitle, repo.IdentityPath())

	// The collision precondition the bug turns on: both titles share one
	// sanitized tmux name, while their derived branches differ.
	if got := tmux.SanitizedNameForRepo(foreignTitle, repo.IdentityPath()); got != sanitized {
		t.Fatalf("collision precondition failed: foreign title sanitizes to %q, want %q", got, sanitized)
	}
	if fb, fb2 := git.BranchForTitle(cfg.BranchPrefix, foreignTitle), git.BranchForTitle(cfg.BranchPrefix, fireTitle); fb == fb2 {
		t.Fatalf("branches must differ so the branch clause cannot catch the foreign row; both = %q", fb)
	}

	// A live tmux session under the colliding name, carrying the af ancestry
	// markers exactly as a normally-created af local session does — so the
	// per-attempt evidence checks pass for it.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession+"="+sanitized,
		"-e", tmux.EnvMarkerGeneration+"=gen-foreign-live",
		"-e", tmux.EnvMarkerHome+"="+home, "sh").CombinedOutput(); err != nil {
		t.Fatalf("seed foreign tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run()
	})

	fire := &taskFireMarker{
		TaskID:     "ab0r7111",
		IntendedAt: time.Now().Add(-time.Minute), // past: evidence's creation-time gate passes
		Title:      fireTitle,
	}
	foreignRow := session.InstanceData{
		ID:       "inst-foreign-live",
		TaskID:   "s0me0ther",
		Title:    foreignTitle,
		Path:     repoPath,
		Branch:   git.BranchForTitle(cfg.BranchPrefix, foreignTitle),
		TmuxName: sanitized, // owns the colliding tmux identity
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7111", ProjectPath: repoPath, Prompt: "p"},
		fire, repo, []session.InstanceData{foreignRow}, time.Now())
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil {
		t.Fatalf("a foreign persisted row owns the colliding tmux name; the leftover must be left alone, got record %+v", row)
	}
	// The foreign live session must still be alive — the reconcile must not
	// have reaped it through the sanitized name.
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("a foreign persisted row owns this tmux name; its live session must not be reaped, but it is gone: %v: %s", err, out)
	}
}

// TestTaskFireLeftoverRecord_ConcurrentCreateNotReaped is the rowless
// concurrent-create regression. The reconcile's "proven ours" evidence is name
// based (AF_SESSION == sanitized name, AF_HOME == this install, session_created
// >= IntendedAt), and the kill is issued by sanitized name with no identity
// binding — so a concurrently created live session (another task's in-flight
// delivery, or a user/API create) whose title sanitizes to the same tmux name
// passes every check while its persisted row is still in flight (the create
// path persists only AFTER sending the prompt). Before the fix the reconcile
// reaped that session silently (nil, nil, no tombstone or quarantine record).
//
// This simulates the window exactly: a fire marker whose intent predates the
// daemon's boot, a live tmux session created AFTER the boot (as a concurrent
// create racing the replay would be), seeded with the exact ancestry markers a
// real af create stamps (AF_SESSION, AF_HOME, AF_SESSION_GEN), and no persisted
// row (rows = nil — the create has spawned the session but not yet persisted).
// The reconcile must NOT kill it.
func TestTaskFireLeftoverRecord_ConcurrentCreateNotReaped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	title := "concurrent-leftover"
	sanitized := tmux.SanitizedNameForRepo(title, repo.IdentityPath())

	// The reconcile bound is the daemon's own boot; capture it BEFORE the
	// concurrent create spawns its session, as a boot replay would.
	reconcileStart := time.Now()
	time.Sleep(1100 * time.Millisecond) // cross a wall-second so session_created strictly postdates the boot's second

	// A live tmux session under the colliding name, carrying the af ancestry
	// markers exactly as a normally-created af local session does — so the
	// per-attempt evidence checks would pass for it on creation time alone.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession+"="+sanitized,
		"-e", tmux.EnvMarkerGeneration+"=gen-concurrent",
		"-e", tmux.EnvMarkerHome+"="+home, "sh").CombinedOutput(); err != nil {
		t.Fatalf("seed concurrent tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run()
	})

	// IntendedAt predates the boot, so the lower bound (session_created >=
	// IntendedAt) passes — exactly the bug: creation time alone cannot
	// distinguish a genuine leftover from a concurrent create.
	fire := &taskFireMarker{
		TaskID:     "ab0r7120",
		IntendedAt: reconcileStart.Add(-time.Minute),
		Title:      title,
	}
	// rows = nil: the concurrent create has not yet persisted its row, so the
	// persisted-row foreign guard cannot see it.
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7120", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil, reconcileStart)
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil && row.UserKilled {
		t.Fatalf("a concurrent create's live session must not be reaped, got a teardown tombstone: %+v", row)
	}
	// The concurrent create's live session must STILL be alive — the reconcile
	// must not have killed it through the sanitized name.
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("a concurrent create's live session was killed by the reconcile (rowless window): %v: %s", err, out)
	}
}

// TestTaskFireLeftoverRecord_TmuxOnlyProvenReap pins the genuine-leftover
// happy path the concurrent-create guard must NOT regress: a tmux-only leftover
// created by the old daemon (predates the boot, carries AF_SESSION_GEN), with no
// worktree, is proven and reaped, and the kill leaves no residue.
func TestTaskFireLeftoverRecord_TmuxOnlyProvenReap(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	title := "tmux-only-leftover"
	sanitized := tmux.SanitizedNameForRepo(title, repo.IdentityPath())
	intendedAt := time.Now().Add(-time.Minute) // intent a minute before the boot
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession+"="+sanitized,
		"-e", tmux.EnvMarkerGeneration+"=gen-tmux-only",
		"-e", tmux.EnvMarkerHome+"="+home, "sh").CombinedOutput(); err != nil {
		t.Fatalf("seed leftover tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run()
	})

	// Cross a wall-second so the leftover's second-granular creation time
	// strictly predates the reconcile bound: the upper bound now rejects the
	// boot's own wall second (a concurrent create racing the boot lands there),
	// so a genuine leftover must be seeded in an earlier second to read as ours.
	time.Sleep(1100 * time.Millisecond)

	// A genuine tmux-only leftover: the intent predates the boot, the session
	// carries the markers a real af create stamps, and the reconcile bound is
	// captured AFTER the leftover was created (as a boot replay sees a session
	// the old daemon left behind). Both bounds pass → proven → reaped.
	fire := &taskFireMarker{
		TaskID:     "ab0r7121",
		IntendedAt: intendedAt,
		Title:      title,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7121", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil, time.Now())
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	// A proven clean tmux-only leftover has no worktree to reap through a
	// tombstone, so the record is nil — its runtime was already stopped.
	if row != nil {
		t.Fatalf("a proven tmux-only leftover must reap to nil (nothing left to record), got %+v", row)
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err == nil {
		t.Fatalf("the proven leftover's runtime must be stopped inside reconcile, still alive: %s", out)
	}
}

// TestTaskFireLeftoverRecord_SessionWithoutGenerationNotReaped pins the
// probe-to-kill identity binding: the evidence now requires an AF_SESSION_GEN
// marker (every af-created session stamps it together with AF_SESSION) so the
// kill can be bound to the exact probed session. A session that carries
// AF_SESSION/AF_HOME but NO generation is not one af launched and cannot be
// re-identified, so it must not be reaped even when its creation time predates
// the boot — failing closed rather than destroying an unverifiable session.
func TestTaskFireLeftoverRecord_SessionWithoutGenerationNotReaped(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoPath := setupTaskRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	title := "no-gen-leftover"
	sanitized := tmux.SanitizedNameForRepo(title, repo.IdentityPath())

	// AF_SESSION and AF_HOME but deliberately NO AF_SESSION_GEN: not an
	// af-created session (af stamps both together), so its identity cannot be
	// bound through the probe-to-kill window.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", sanitized,
		"-e", tmux.EnvMarkerSession+"="+sanitized,
		"-e", tmux.EnvMarkerHome+"="+home, "sh").CombinedOutput(); err != nil {
		t.Fatalf("seed tmux session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", "="+sanitized).Run()
	})

	fire := &taskFireMarker{
		TaskID: "ab0r7122", IntendedAt: time.Now().Add(-time.Minute), Title: title,
	}
	row, err := taskFireLeftoverRecord(
		&task.Task{ID: "ab0r7122", ProjectPath: repoPath, Prompt: "p", Program: "claude"},
		fire, repo, nil, time.Now())
	if err != nil {
		t.Fatalf("taskFireLeftoverRecord: %v", err)
	}
	if row != nil && row.UserKilled {
		t.Fatalf("a session af cannot re-identify (no generation) must not be reaped, got a tombstone: %+v", row)
	}
	if out, err := exec.Command("tmux", "has-session", "-t", "="+sanitized).CombinedOutput(); err != nil {
		t.Fatalf("the unverifiable session must not be killed: %v: %s", err, out)
	}
}

// installFakeTmuxForEvidence installs a fake `tmux` on PATH that answers the
// four probes taskFireTmuxEvidence issues — show-environment for AF_HOME,
// AF_SESSION, AF_SESSION_GEN, and display-message for session_created — so the
// evidence logic (the reconcileStart upper bound and the generation
// requirement) can be exercised hermetically on hosts that have no real tmux
// server. gen == "" makes the fake report AF_SESSION_GEN absent (the targeted
// query exits 1 with tmux's exact "unknown variable" stderr, then the
// unfiltered fallback succeeds, matching how a real tmux answers). created is
// the unix seconds display-message echoes. The fake lives for the test only
// (PATH is restored on cleanup), so it cannot disturb a shared host's tmux.
func installFakeTmuxForEvidence(t *testing.T, gen string, created int64) {
	t.Helper()
	home := os.Getenv("AGENT_FACTORY_HOME")
	if home == "" {
		t.Fatal("precondition: AGENT_FACTORY_HOME must be set before installing the fake tmux")
	}
	// The gen branch is selected at script-generation time: when gen == "" the
	// fake emits tmux's exact "unknown variable: AF_SESSION_GEN" on stderr and
	// exits 1, then succeeds on the unfiltered fallback — matching how a real
	// tmux answers an absent marker (#3309). When gen != "" it echoes the
	// marker line. No runtime `if` so the two responses cannot be conflated by
	// shell quoting.
	genBranch := "echo \"AF_SESSION_GEN=" + gen + "\""
	if gen == "" {
		genBranch = "echo \"unknown variable: AF_SESSION_GEN\" >&2; exit 1"
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"n=${3#=}\n" +
		"n=${n%:}\n" +
		"case \"$1\" in\n" +
		"show-environment)\n" +
		"  if [ -z \"$4\" ]; then exit 0; fi\n" +
		"  case \"$4\" in\n" +
		"    AF_HOME) echo \"AF_HOME=" + home + "\" ;;\n" +
		"    AF_SESSION) echo \"AF_SESSION=$n\" ;;\n" +
		"    AF_SESSION_GEN) " + genBranch + " ;;\n" +
		"    *) echo \"unknown variable: $4\" >&2; exit 1 ;;\n" +
		"  esac\n" +
		"  ;;\n" +
		"display-message) echo \"" + fmt.Sprintf("%d", created) + "\" ;;\n" +
		"*) exit 0 ;;\n" +
		"esac\n"
	t.Logf("fake tmux script:\n%s", script)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestTaskFireTmuxEvidence_ReconcileStartBound pins the upper bound that closes
// the rowless concurrent-create window, hermetically (no real tmux needed). A
// session whose server-side creation time postdates the reconcile bound — as a
// concurrent create racing the boot replay does — must never read as "proven",
// no matter that its ancestry markers all match. The lower bound alone
// (session_created >= IntendedAt) cannot distinguish a genuine leftover from a
// concurrent create; the upper bound (session_created not-after the daemon's
// own boot) does, because a true leftover was created by the PREVIOUS daemon.
func TestTaskFireTmuxEvidence_ReconcileStartBound(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	reconcileStart := time.Now()
	sanitized := "af_fake_evidence"

	// Session created a minute AFTER the boot: a concurrent create racing the
	// replay. Its markers all match (AF_SESSION/AF_HOME/AF_SESSION_GEN present and
	// correct) and it postdates the intent, so the lower bound alone passes —
	// only the upper bound excludes it.
	createdAfter := reconcileStart.Add(time.Minute).Unix()
	installFakeTmuxForEvidence(t, "gen-after", createdAfter)
	fire := &taskFireMarker{TaskID: "ab0r7130", IntendedAt: reconcileStart.Add(-time.Hour)}
	proven, gen := taskFireTmuxEvidence(cmd.MakeExecutor(), sanitized, fire, reconcileStart)
	if proven || gen != "" {
		t.Fatalf("a session created after the reconcile bound must not be proven, got proven=%v gen=%q", proven, gen)
	}

	// Session created a minute AFTER the intent but a minute BEFORE the boot:
	// a genuine leftover. Both bounds pass and the generation is returned so the
	// caller can bind the kill to it.
	createdBefore := reconcileStart.Add(-time.Minute).Unix()
	installFakeTmuxForEvidence(t, "gen-before", createdBefore)
	proven, gen = taskFireTmuxEvidence(cmd.MakeExecutor(), sanitized, fire, reconcileStart)
	if !proven {
		t.Fatalf("a genuine leftover predating the boot must be proven, got proven=%v", proven)
	}
	if gen != "gen-before" {
		t.Fatalf("a proven session must return its generation for kill binding, got %q", gen)
	}
}

// TestTaskFireTmuxEvidence_RequiresGeneration pins the probe-to-kill identity
// requirement: every af-created session stamps AF_SESSION and AF_SESSION_GEN
// together, so a session carrying AF_SESSION/AF_HOME but NO generation is not
// one af launched and cannot be re-identified through the probe-to-kill window.
// Evidence must fail closed rather than authorize a kill it cannot bind to the
// probed session. Hermetic (no real tmux).
func TestTaskFireTmuxEvidence_RequiresGeneration(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	now := time.Now()
	sanitized := "af_fake_nogen"
	// A session predating the boot (genuine-leftover timing) but with no
	// AF_SESSION_GEN marker: af cannot re-identify it, so it is not proven.
	installFakeTmuxForEvidence(t, "", now.Add(-time.Minute).Unix())
	fire := &taskFireMarker{TaskID: "ab0r7131", IntendedAt: now.Add(-time.Hour)}
	proven, gen := taskFireTmuxEvidence(cmd.MakeExecutor(), sanitized, fire, now)
	if proven || gen != "" {
		t.Fatalf("a session without AF_SESSION_GEN must not be proven, got proven=%v gen=%q", proven, gen)
	}
}

// TestTaskFireTmuxEvidence_HomeMismatchNotProven pins the other half of the
// provenance: AF_HOME must point at THIS install. Hermetic (no real tmux): the
// fake echoes an AF_HOME that does not equal the test's AGENT_FACTORY_HOME,
// so ownership fails and the session is left alone regardless of timing.
func TestTaskFireTmuxEvidence_HomeMismatchNotProven(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	now := time.Now()
	sanitized := "af_fake_otherhome"
	// Install a fake whose AF_HOME answer is a different directory than the
	// test's home.
	dir := t.TempDir()
	otherHome := filepath.Join(dir, "other-af-home")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"show-environment) echo \"AF_HOME=" + otherHome + "\" ;;\n" +
		"display-message) echo \"" + fmt.Sprintf("%d", now.Add(-time.Minute).Unix()) + "\" ;;\n" +
		"*) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fire := &taskFireMarker{TaskID: "ab0r7132", IntendedAt: now.Add(-time.Hour)}
	proven, gen := taskFireTmuxEvidence(cmd.MakeExecutor(), sanitized, fire, now)
	if proven || gen != "" {
		t.Fatalf("a session whose AF_HOME is not this install must not be proven, got proven=%v gen=%q", proven, gen)
	}
}

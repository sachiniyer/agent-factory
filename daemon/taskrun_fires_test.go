package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
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
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7004",
		IntendedAt: time.Now().Add(-time.Minute),
		Title:      "nightly-2",
		InstanceID: "inst-abc",
	})

	if err := runTaskDelivery("ab0r7004", task.ProjectExpectation{}, true); err != nil {
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

	if err := runTaskDelivery("ab0r7005", task.ProjectExpectation{}, true); err != nil {
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

	if err := runTaskDelivery("ab0r7006", task.ProjectExpectation{}, true); err != nil {
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

	if err := runTaskDelivery("ab0r7007", task.ProjectExpectation{}, true); err != nil {
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

	if err := runTaskDelivery("ab0r7008", task.ProjectExpectation{}, false); err != nil {
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

	if err := runTaskDelivery("ab0r7009", task.ProjectExpectation{}, true); err != nil {
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
	seedPendingFire(t, taskFireMarker{
		TaskID:     "ab0r7010",
		IntendedAt: time.Now().Add(-time.Minute),
		Title:      "nightly-2",
	})

	if err := runTaskDelivery("ab0r7010", task.ProjectExpectation{}, true); err != nil {
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

// TestRunTask_ReplayWithoutMarkerDeliversNormally pins that the replay flag
// alone changes nothing for a task with no pending fire.
func TestRunTask_ReplayWithoutMarkerDeliversNormally(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repo := setupTaskRepo(t)
	seedCronTask(t, "ab0r7014", repo)
	creates, _ := stubTaskDelivery(t)

	if err := runTaskDelivery("ab0r7014", task.ProjectExpectation{}, true); err != nil {
		t.Fatalf("runTaskDelivery: %v", err)
	}
	if len(*creates) != 1 {
		t.Fatalf("a first-ever run must deliver; creates = %d", len(*creates))
	}
}

// TestTaskFireExpectedWorktreePath pins the tombstone's recorded location to the
// same placement the interrupted create would have used.
func TestTaskFireExpectedWorktreePath(t *testing.T) {
	sibling := taskFireExpectedWorktreePath(&config.Config{}, "/home/u/repo", "my-title", "dev/my-title")
	want := filepath.Join("/home/u", "repo-my-title")
	if sibling != want {
		t.Fatalf("sibling path = %q, want %q", sibling, want)
	}
	sub := taskFireExpectedWorktreePath(
		&config.Config{WorktreeRoot: config.WorktreeRootSubdirectory},
		"/home/u/repo", "My Title", "dev/my-title")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	if want := filepath.Join(home, "worktrees", "dev/my-title"); sub != want {
		t.Fatalf("subdirectory path = %q, want %q", sub, want)
	}
}

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// These pin the delivery-failure alarm's repo scope against the #1238/#2719
// drift bug: newTaskWatcher used to derive w.repoID from config.RepoFromPath at
// construction time, so a watch task bound to repo A (t.RepoID = A) whose
// ProjectPath now resolves to repo B — a symlink retarget, re-derived on a
// daemon restart — had its alarm routed to B's snapshot (or to no repo-scoped
// snapshot at all for an unresolvable path), while the TUI listed the task
// under A per its retained RepoID. The owner of the dead task never saw the
// alarm, and the foreign repo's operator saw a banner for a task that was not
// theirs. The fix scopes by the authoritative retained RepoID, falling back to
// the historical path-derived scope only for a legacy/unbound row, matching the
// rule the task-list scoping already uses (task/repo_scope.go,
// app/remote_mode.go filterTasksToProject).

// mkAlarmRepo creates a throwaway git repository and returns its real
// (symlink-resolved) root. Two calls produce two distinct repo identities, so a
// symlink retargeted from one to the other changes what RepoFromPath resolves
// the path to — the precondition for the drift manifestation.
func mkAlarmRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	require.NoError(t, exec.Command("git", "init", repo).Run(), "git init %s", name)
	real, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	return real
}

// alarmRepoID resolves a repo's ID through the same path the delivery boundary
// uses, so a test asserts the authoritative identity rather than a hand-rolled
// hash.
func alarmRepoID(t *testing.T, path string) string {
	t.Helper()
	repo, err := config.RepoFromPath(path)
	require.NoError(t, err)
	return repo.ID
}

// driveAlarmPastThreshold feeds a watcher the consecutive failed deliveries that
// arm its delivery-failure alarm, going through recordDeliveryResult (the
// production bookkeeping path) rather than injecting state directly. It returns
// the "now" at which the alarm has persisted past the threshold.
func driveAlarmPastThreshold(t *testing.T, w *taskWatcher, failErr error) time.Time {
	t.Helper()
	t0 := time.Now()
	w.recordDeliveryResult(t0, failErr)
	now := t0.Add(watcherDeliveryAlarmThreshold + time.Minute)
	w.recordDeliveryResult(now, failErr)
	return now
}

// registerWatcher inserts a watcher constructed via newTaskWatcher into the
// supervisor's map. newTaskWatcher builds the struct but does NOT register it
// (reconcile/restart own registration in production), so a test that asserts on
// deliveryAlarms must add it the same way watcher_drop_replacement_test.go does
// — otherwise the projection iterates an empty map. doneCh is pre-closed so
// newTestSupervisor's t.Cleanup(s.Stop) does not block on <-doneCh for a
// watcher whose run goroutine was never started.
func registerWatcher(t *testing.T, s *watcherSupervisor, w *taskWatcher) {
	t.Helper()
	close(w.doneCh)
	s.mu.Lock()
	s.watchers[w.taskID] = w
	s.mu.Unlock()
}

// TestNewTaskWatcher_AlarmScopeFollowsRetainedRepoIDNotDriftedPath is the
// headline #1238/#2719 repro: a watch task bound to repo A whose ProjectPath is
// a symlink that now resolves to repo B must STILL scope its delivery-failure
// alarm to A — the retained binding the TUI task list and the delivery boundary
// both treat as authoritative. With the bug, newTaskWatcher re-derived the
// scope from the drifted path and routed the alarm to B, while the TUI listed
// the task under A: the owner of the dead task never saw the alarm and the other
// repo's operator saw a foreign banner.
func TestNewTaskWatcher_AlarmScopeFollowsRetainedRepoIDNotDriftedPath(t *testing.T) {
	repoA := mkAlarmRepo(t, "a")
	repoB := mkAlarmRepo(t, "b")
	idA := alarmRepoID(t, repoA)
	idB := alarmRepoID(t, repoB)
	require.NotEqual(t, idA, idB, "the two repos must have distinct identities")

	// Bound path starts as a symlink to A; the task is bound to A (retained ID).
	bound := filepath.Join(t.TempDir(), "bound")
	require.NoError(t, os.Symlink(repoA, bound))
	tBound := watchTask("drift0001", "sleep 3600", bound)
	tBound.RepoID = idA

	s, _ := newTestSupervisor(t, staticTasks())

	// Before drift: the alarm scope is A. (Path-derived and retained agree here.)
	w := s.newTaskWatcher(tBound)
	require.Equal(t, idA, w.repoID,
		"a task bound to A with a path that still resolves to A must scope its alarm to A")

	// Drift: retarget the symlink to B, then re-derive the watcher — the daemon
	// restart / watcher rebuild path, the only producer of the mismatch state.
	require.NoError(t, os.Remove(bound))
	require.NoError(t, os.Symlink(repoB, bound))
	// Sanity: the path now resolves to B — exactly the state the delivery
	// boundary's rebind refusal defends against (daemon/delivery.go).
	require.Equal(t, idB, alarmRepoID(t, bound),
		"the symlink retarget must change what the path resolves to")

	w2 := s.newTaskWatcher(tBound) // same task: t.RepoID still A, path now resolves to B
	require.Equal(t, idA, w2.repoID,
		"the alarm scope must follow the retained RepoID, not the drifted path "+
			"(a task bound to A must not route its alarm to B)")

	// Arm the alarm and assert it lands in A's snapshot (the owner) and does
	// NOT leak into B's (the foreign repo).
	registerWatcher(t, s, w2)
	now := driveAlarmPastThreshold(t, w2, errors.New("target session down"))
	require.Len(t, s.deliveryAlarms(idA, now), 1,
		"the owner repo A must see its own task's delivery alarm")
	require.Empty(t, s.deliveryAlarms(idB, now),
		"the foreign repo B must not see a task it does not own")
	require.Len(t, s.deliveryAlarms("", now), 1,
		"the all-repos read still surfaces the alarm, scoped under its owner")
}

// TestNewTaskWatcher_AlarmScopeRetainedRepoIDSurvivesUnresolvablePath covers the
// second manifestation: a task bound to A whose ProjectPath no longer resolves to
// ANY repo. With the bug, newTaskWatcher left w.repoID empty and the alarm was
// hidden from every repo-scoped snapshot — only the all-repos read saw it. The
// retained binding survives the vanished path by design (task.Task.RepoID), so
// the fix routes the alarm to A.
func TestNewTaskWatcher_AlarmScopeRetainedRepoIDSurvivesUnresolvablePath(t *testing.T) {
	repoA := mkAlarmRepo(t, "a")
	idA := alarmRepoID(t, repoA)

	// A path that does not resolve to any repo (never created on disk).
	gone := filepath.Join(t.TempDir(), "vanished")
	tBound := watchTask("gone0001", "sleep 3600", gone)
	tBound.RepoID = idA

	s, _ := newTestSupervisor(t, staticTasks())
	w := s.newTaskWatcher(tBound)
	require.Equal(t, idA, w.repoID,
		"the retained RepoID must scope the alarm even when the path cannot resolve")
	require.NotEmpty(t, w.repoID,
		"the scope must not collapse to empty for a bound task with a vanished path")

	registerWatcher(t, s, w)
	now := driveAlarmPastThreshold(t, w, errors.New("target session down"))
	require.Len(t, s.deliveryAlarms(idA, now), 1,
		"repo A must see its own task's alarm — the outage the alarm exists to surface")
	require.Len(t, s.deliveryAlarms("", now), 1,
		"the all-repos read also sees it, now under its true owner")
}

// TestNewTaskWatcher_AlarmScopeFallsBackToPathForLegacyUnboundRow pins the
// legacy fallback the fix preserves: a row written before Task.RepoID existed
// (RepoID empty) keeps its historical path-derived alarm scope. This is the
// same rule the task-list scoping uses, so the alarm and the task list still
// agree for every legacy row — no regression in the common single-repo case.
func TestNewTaskWatcher_AlarmScopeFallsBackToPathForLegacyUnboundRow(t *testing.T) {
	repoA := mkAlarmRepo(t, "a")
	idA := alarmRepoID(t, repoA)

	// Legacy row: no retained binding, path resolves to a real repo. t.RepoID
	// intentionally left empty, as a pre-#2719 row carries it.
	legacy := watchTask("legac001", "sleep 3600", repoA)

	s, _ := newTestSupervisor(t, staticTasks())
	w := s.newTaskWatcher(legacy)
	require.Equal(t, idA, w.repoID,
		"a legacy unbound row keeps its historical path-derived alarm scope")

	// And the alarm then lands in that repo's snapshot, as it did before.
	registerWatcher(t, s, w)
	now := driveAlarmPastThreshold(t, w, errors.New("target session down"))
	require.Len(t, s.deliveryAlarms(idA, now), 1,
		"a legacy row's alarm still surfaces in the repo its path resolves to")
}

// TestNewTaskWatcher_AlarmScopeLegacyUnboundUnresolvablePathLeavesEmpty pins the
// degenerate legacy case the fix leaves untouched: a row with neither a
// retained binding nor a resolvable path keeps no repo scope — only the
// all-repos read sees it — exactly as before the fix, since no authoritative
// identity exists to scope by.
func TestNewTaskWatcher_AlarmScopeLegacyUnboundUnresolvablePathLeavesEmpty(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "vanished")
	legacy := watchTask("noop0001", "sleep 3600", gone)
	// Both RepoID and a resolvable path are absent, as a pre-#2719 row orphaned
	// by filesystem deletion would carry.

	s, _ := newTestSupervisor(t, staticTasks())
	w := s.newTaskWatcher(legacy)
	require.Empty(t, w.repoID,
		"with no binding and no resolvable path the alarm keeps no repo scope")

	registerWatcher(t, s, w)
	now := driveAlarmPastThreshold(t, w, errors.New("target session down"))
	require.Empty(t, s.deliveryAlarms("any-repo-not-ours", now),
		"a repo-scoped read sees nothing for a scope-less legacy alarm")
	require.Len(t, s.deliveryAlarms("", now), 1,
		"only the all-repos read surfaces a scope-less legacy alarm")
}

package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The #2868 regression lock: a Lost-restore failure episode belongs to ONE
// runtime, and a later session that merely reuses the title must not inherit it.
//
// The field shape: a session goes Lost, its restores fail four times, the user
// gives up and kills it, then creates a fresh session with the same name. The new
// session's very first hiccup was charged as failure five — so it waited minutes
// for a retry it should have gotten in ten seconds, born broken for reasons
// belonging to a session the user had already discarded. The cause was keying:
// lostRestoreStates was filed under the repo/title daemon key, which addresses a
// SLOT that outlives its occupant, while every other per-runtime map (the
// remote-loss debounce, aliveObservations, handoff retries) had already been moved
// to the stable instance id for exactly this reason.
//
// Both halves of the cycle are driven here because af offers two ways to free a
// title and they leave the manager map in different shapes: a kill REMOVES the
// row, while reuse-archived-name RENAMES it and leaves it in place (see
// renameArchivedForReuseLocked). A fix that only reaped state for rows that
// vanished would still hand the archived session's history to its successor.

// failEveryRestoreBackend fails every Recover, which is what an episode of
// consecutive failures is made of.
type failEveryRestoreBackend struct {
	*session.FakeBackend
	err error
}

func (b *failEveryRestoreBackend) Recover(*session.Instance) error { return b.err }
func (b *failEveryRestoreBackend) Type() string                    { return "local" }

// trackedEpisodes summarizes every retry episode the manager holds — the failure
// counts, sorted, and the furthest-out retry deadline among them.
//
// It deliberately names NO key. Which key an episode is filed under is the thing
// under test, so an assertion that looked one up by key would pass or fail for the
// wrong reason: against the buggy build it would read a key that build never
// writes and fail as a missing precondition rather than as an inherited backoff.
// Asked this way, the same assertion means the same thing on both builds — how
// many episodes exist and how much each one has accumulated.
func trackedEpisodes(m *Manager) (failures []int, longestWait time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	failures = make([]int, 0, len(m.lostRestoreStates))
	for _, st := range m.lostRestoreStates {
		failures = append(failures, st.consecutiveFailures)
		if wait := time.Until(st.nextAttempt); wait > longestWait {
			longestWait = wait
		}
	}
	slices.Sort(failures)
	return failures, longestWait
}

// soleEpisode returns the one episode being tracked, failing the test if there is
// not exactly one. Key-agnostic for the same reason trackedEpisodes is.
func soleEpisode(t *testing.T, m *Manager) *lostRestoreState {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.lostRestoreStates) != 1 {
		t.Fatalf("tracked episodes = %d, want exactly 1", len(m.lostRestoreStates))
	}
	for _, st := range m.lostRestoreStates {
		return st
	}
	return nil
}

// accumulateRestoreFailures drives n failed restore passes against a Lost session
// with the backoff collapsed, then puts the real schedule back so the NEXT
// failure's retry deadline is a duration a user would actually wait.
func accumulateRestoreFailures(t *testing.T, m *Manager, n int) {
	t.Helper()
	realBase := lostRestoreBackoffBase
	lostRestoreBackoffBase = 0
	t.Cleanup(func() { lostRestoreBackoffBase = realBase })
	for i := 0; i < n; i++ {
		m.RestoreLostSessions()
	}
	lostRestoreBackoffBase = realBase

	failures, _ := trackedEpisodes(m)
	if !slices.Equal(failures, []int{n}) {
		t.Fatalf("precondition: after %d failed restores the tracked episodes are %v, want exactly one with %d",
			n, failures, n)
	}
}

// assertNewSessionStartsFresh is the whole point of the fix, asserted twice: once
// on the counter, and once on the thing the user actually experiences — how long
// the new session waits before anyone tries to restore it again.
func assertNewSessionStartsFresh(t *testing.T, m *Manager) {
	t.Helper()
	failures, longestWait := trackedEpisodes(m)
	if !slices.Equal(failures, []int{1}) {
		t.Fatalf("tracked episodes after the NEW session's FIRST failed restore = %v, want exactly "+
			"one episode with one failure: the new session inherited the failure history of a session "+
			"the user already discarded, because the episode was filed under the reused TITLE rather "+
			"than the stable instance id (#2868)", failures)
	}
	// backoff(1) is lostRestoreBackoffBase (10s); backoff(5), the inherited count,
	// is 160s. Allowing 2x the base keeps this about the escalation rather than
	// about scheduling precision.
	if longestWait > 2*lostRestoreBackoffBase {
		t.Fatalf("the new session waits %s for its next restore attempt, want no more than %s: an "+
			"inherited episode put it deep into the exponential backoff on its first failure (#2868)",
			longestWait.Round(time.Second), (2 * lostRestoreBackoffBase).Round(time.Second))
	}
}

// TestRestoreLostSessions_KilledThenTitleReused_StartsFreshEpisode drives the
// reported cycle: Lost -> restores fail -> user kills it -> new session takes the
// freed title. The new session's first failure must be its first, not the old
// session's fifth.
func TestRestoreLostSessions_KilledThenTitleReused_StartsFreshEpisode(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	doomed := &failEveryRestoreBackend{
		FakeBackend: session.NewFakeBackend(),
		err:         errors.New("worktree is gone"),
	}
	old := registerStarted(t, manager, repoID, repoPath, "recycled", doomed, true, session.Lost)
	accumulateRestoreFailures(t, manager, 4)

	// The user gives up and kills it: the row leaves the manager map once teardown
	// commits.
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "recycled"))
	manager.mu.Unlock()

	// ...and creates a new session with the same name. Same title, different
	// session — which is precisely what the stable id exists to tell apart (#1195).
	fresh := &failEveryRestoreBackend{
		FakeBackend: session.NewFakeBackend(),
		err:         errors.New("a transient blip"),
	}
	replacement := registerStarted(t, manager, repoID, repoPath, "recycled", fresh, true, session.Lost)
	if replacement.ID == old.ID {
		t.Fatal("precondition: a new session must mint a new stable id")
	}

	manager.RestoreLostSessions()

	// Exactly one episode with exactly one failure: the new session's own. That
	// single shape carries both halves — nothing was inherited, and the discarded
	// session's episode was reaped rather than accumulating for the daemon's life.
	assertNewSessionStartsFresh(t, manager)
}

// TestRestoreLostSessions_ArchivedThenTitleReused_StartsFreshEpisode is the other
// way a title comes free, and the harder one: reuse-archived-name RENAMES the
// archived row and leaves it in the manager map, so the successor moves in under a
// key whose retry state was never reaped.
func TestRestoreLostSessions_ArchivedThenTitleReused_StartsFreshEpisode(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	doomed := &failEveryRestoreBackend{
		FakeBackend: session.NewFakeBackend(),
		err:         errors.New("worktree is gone"),
	}
	old := registerStarted(t, manager, repoID, repoPath, "recycled", doomed, true, session.Lost)
	accumulateRestoreFailures(t, manager, 4)

	// The user archives it, then creates a new session with the same name.
	// renameArchivedForReuseLocked frees the title by renaming the archived row and
	// RE-KEYING it in the manager map — the row stays, under a new key. Modelled
	// here rather than called: the real helper relocates a worktree and moves a
	// branch, none of which this episode's keying depends on.
	if err := old.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("archive transition: %v", err)
	}
	old.SetStartedForTest(false)
	if err := old.SetTitle("recycled-1"); err != nil {
		t.Fatalf("rename archived: %v", err)
	}
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "recycled"))
	manager.instances[daemonInstanceKey(repoID, "recycled-1")] = old
	manager.mu.Unlock()

	fresh := &failEveryRestoreBackend{
		FakeBackend: session.NewFakeBackend(),
		err:         errors.New("a transient blip"),
	}
	registerStarted(t, manager, repoID, repoPath, "recycled", fresh, true, session.Lost)

	manager.RestoreLostSessions()
	assertNewSessionStartsFresh(t, manager)
}

// TestRestoreLostSessions_ArchivedMidConfirmation_DropsItsEpisode covers the one
// case the "no longer Lost" sweep cannot: a restore whose spawn succeeded is held
// awaiting a liveness OBSERVATION (#1910), and the session is archived before any
// poll answers. Archiving tears the runtime down, so that observation is never
// coming — and the row stays in the manager map under its stable id, so presence
// alone would keep the episode for the daemon's whole life and greet the session
// with a stale backoff if it were ever restored. Same rule, same reason, as
// sweepRemoteLossStates: unstarted or Archived is not live.
func TestRestoreLostSessions_ArchivedMidConfirmation_DropsItsEpisode(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "shelved", backend, true, session.Lost)

	manager.RestoreLostSessions() // recovers; the episode is held awaiting confirmation
	if !soleEpisode(t, manager).awaitingConfirm {
		t.Fatal("precondition: a successful respawn must leave the episode awaiting confirmation")
	}

	// Archived before any poll got an answer out of the new runtime.
	if err := inst.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("archive transition: %v", err)
	}
	inst.SetStartedForTest(false)

	manager.RestoreLostSessions()

	if failures, _ := trackedEpisodes(manager); len(failures) != 0 {
		t.Fatalf("tracked episodes after archiving = %v, want none: the runtime those failures "+
			"describe was deliberately torn down, so no poll will ever confirm it alive and clear "+
			"the entry", failures)
	}
}

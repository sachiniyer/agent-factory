package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The #2876 regression lock, the usage-limit sibling of #2868: an auto-resume
// retry episode belongs to ONE runtime, and a later session that merely reuses
// the title must not inherit it.
//
// This one bites harder than #2868 did, because the inherited field is a GATE
// rather than a counter. limitResumeState.nextAttempt is consulted as "nothing
// may fire before this", so a successor that moves into a title whose predecessor
// was mid-backoff is not merely slow to be resumed — it is not resumed AT ALL
// until the discarded session's backoff expires, and nothing is logged. A user
// watching a fresh session sit parked at a usage-limit wall sees the auto-resume
// feature simply not work.
//
// Why the sweep does not catch it: an entry is dropped only once the session has
// left LiveLimitReached AND its backoff has elapsed. During a backoff the second
// clause is false by construction, and the reap loop below it keys off
// m.instances, which a same-titled successor repopulates.

// newTitleReuseLimitManager builds the auto-resume fixture and also hands back the
// repo path, so a SECOND session can be registered under the same title later —
// which newAutoResumeManager, built for single-session tests, cannot do.
func newTitleReuseLimitManager(t *testing.T) (*Manager, string, string, *session.Instance, *limitResumeBackend) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	manager.cfg.LimitAutoResume = true
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "old work"
	inst.SetLimitReached(nowFunc()) // reset already in the past
	return manager, repoID, repoPath, inst, backend
}

// limitEpisodeAttempts reports the attempt count of every auto-resume episode the
// manager holds, sorted. Key-agnostic for the same reason trackedEpisodes is (see
// lostrestore_title_reuse_test.go): which key an episode is filed under is the
// thing under test, so a by-key lookup would fail against the unfixed build as a
// missing precondition rather than as inherited state.
func limitEpisodeAttempts(m *Manager) []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	attempts := make([]int, 0, len(m.limitResumeStates))
	for _, st := range m.limitResumeStates {
		attempts = append(attempts, st.attempts)
	}
	slices.Sort(attempts)
	return attempts
}

// parkSuccessor registers a second session under the same title the predecessor
// held, parked at a usage-limit wall whose window has already elapsed — so it is
// due for auto-resume immediately, and any delay it suffers is inherited.
func parkSuccessor(t *testing.T, m *Manager, repoID, repoPath, title string) (*session.Instance, *limitResumeBackend) {
	t.Helper()
	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, m, repoID, repoPath, title, backend, true, session.Running)
	inst.Prompt = "the successor's own work"
	inst.SetLimitReached(nowFunc().Add(-time.Hour))
	return inst, backend
}

// assertSuccessorResumesOnItsOwnSchedule is the user-visible assertion: the new
// session is auto-resumed on ITS due time, and the episode recording that is its
// own first attempt — not a continuation of a discarded session's backoff.
func assertSuccessorResumesOnItsOwnSchedule(t *testing.T, m *Manager, backend *limitResumeBackend) {
	t.Helper()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("the NEW session was resumed %d time(s), want exactly 1: its own limit window had "+
			"elapsed, so auto-resume was due — it was gated behind the backoff of a session the user "+
			"already discarded, because the episode was filed under the reused TITLE rather than the "+
			"stable instance id (#2876)", len(prompts))
	}
	if attempts := limitEpisodeAttempts(m); !slices.Equal(attempts, []int{1}) {
		t.Fatalf("tracked auto-resume episodes = %v, want exactly one with one attempt: the successor "+
			"must start its own episode, and the discarded session's must not outlive it (#2876)", attempts)
	}
}

// TestResumeLimitedSessions_KilledThenTitleReused_ResumesOnOwnSchedule drives the
// reported cycle: parked at a limit -> auto-resume fires and arms a backoff ->
// user kills the session -> a new session takes the freed title and parks at a
// limit of its own. The new session must be resumed when ITS window elapses.
func TestResumeLimitedSessions_KilledThenTitleReused_ResumesOnOwnSchedule(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, repoPath, old, oldBackend := newTitleReuseLimitManager(t)

	// One auto-resume fires and arms the exponential backoff.
	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := oldBackend.snapshot(); len(prompts) != 1 {
		t.Fatalf("precondition: the predecessor must be resumed once, got %d", len(prompts))
	}
	if attempts := limitEpisodeAttempts(manager); !slices.Equal(attempts, []int{1}) {
		t.Fatalf("precondition: tracked episodes = %v, want exactly one with one attempt", attempts)
	}

	// The user kills it WHILE that backoff is still running — which is exactly when
	// the entry cannot be swept, since the sweep requires the backoff to have
	// elapsed before it will drop one.
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "limited"))
	manager.mu.Unlock()

	replacement, freshBackend := parkSuccessor(t, manager, repoID, repoPath, "limited")
	if replacement.ID == old.ID {
		t.Fatal("precondition: a new session must mint a new stable id")
	}

	// Still inside the predecessor's 10s backoff.
	advance(time.Second)
	manager.ResumeLimitedSessions()
	assertSuccessorResumesOnItsOwnSchedule(t, manager, freshBackend)
}

// TestResumeLimitedSessions_ArchivedThenTitleReused_ResumesOnOwnSchedule is the
// other route to a free title: reuse-archived-name RENAMES the archived row and
// leaves it in the manager map, so the successor moves into a key whose episode
// nothing reaped.
func TestResumeLimitedSessions_ArchivedThenTitleReused_ResumesOnOwnSchedule(t *testing.T) {
	advance := withFrozenClock(t)
	manager, repoID, repoPath, old, oldBackend := newTitleReuseLimitManager(t)

	advance(limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()
	if _, _, prompts := oldBackend.snapshot(); len(prompts) != 1 {
		t.Fatalf("precondition: the predecessor must be resumed once, got %d", len(prompts))
	}

	// Archived and renamed out of the way, as renameArchivedForReuseLocked does.
	if err := old.Transition(session.ObserveLiveness(session.LiveArchived)); err != nil {
		t.Fatalf("archive transition: %v", err)
	}
	old.SetStartedForTest(false)
	if err := old.SetTitle("limited-1"); err != nil {
		t.Fatalf("rename archived: %v", err)
	}
	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, "limited"))
	manager.instances[daemonInstanceKey(repoID, "limited-1")] = old
	manager.mu.Unlock()

	_, freshBackend := parkSuccessor(t, manager, repoID, repoPath, "limited")

	advance(time.Second)
	manager.ResumeLimitedSessions()
	assertSuccessorResumesOnItsOwnSchedule(t, manager, freshBackend)
}

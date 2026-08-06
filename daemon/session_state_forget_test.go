package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #3031: aliveObservations grew for every session ever created and was never
// pruned, so a daemon that "runs forever" accumulated one entry per session for
// its whole lifetime.
//
// Asserted by COUNT, not by a memory reading. Resident set on the maintainer's
// box was 47 MB after 448 sessions — far too noisy to pin this — while the map
// length is exact and states the property directly: after N sessions are created
// and killed, the map is back where it started.
func TestForgetSessionRuntimeState_ReturnsToBaseline(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	baselineAlive := len(manager.aliveObservations)
	baselineHandoff := len(manager.handoffRetryDue)

	const sessions = 50
	instances := make([]*session.Instance, 0, sessions)
	for i := 0; i < sessions; i++ {
		inst, _ := registerArchivable(t, manager, repoID, repoPath, sessionTitleForIndex(i))
		instances = append(instances, inst)

		// Exactly what the poll does when it gets an answer out of a runtime, and
		// what ResumePendingHandoffs does when it defers a retry.
		manager.mu.Lock()
		key := stableSessionKey(repoID, inst)
		manager.aliveObservations[key]++
		manager.handoffRetryDue[key] = nowFunc()
		manager.mu.Unlock()
	}

	manager.mu.Lock()
	grewAlive := len(manager.aliveObservations)
	manager.mu.Unlock()
	require.Equal(t, baselineAlive+sessions, grewAlive,
		"precondition: the observations must actually accumulate, or this test would pass without the leak existing")

	for i, inst := range instances {
		manager.mu.Lock()
		key := daemonInstanceKey(repoID, sessionTitleForIndex(i))
		delete(manager.instances, key)
		manager.forgetSessionRuntimeStateLocked(repoID, inst)
		manager.mu.Unlock()
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	require.Equal(t, baselineAlive, len(manager.aliveObservations),
		"aliveObservations must return to its starting size once every session is gone; a surviving entry is the leak")
	require.Equal(t, baselineHandoff, len(manager.handoffRetryDue),
		"handoffRetryDue orphans the same way: its only clear runs from a loop over m.instances, which never walks a removed session")
}

// The other half of a correct lifetime. A cap or an LRU would also keep the map
// small, and would be wrong: aliveObservations is what the Lost-restore loop
// reads to decide whether a session was ever seen alive after spawn, so evicting
// a LIVE session's entry reads as "never observed" and suppresses a legitimate
// restore. Deleting only on record removal cannot do that.
func TestForgetSessionRuntimeState_KeepsLiveSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	keep, _ := registerArchivable(t, manager, repoID, repoPath, "keep-me")
	drop, _ := registerArchivable(t, manager, repoID, repoPath, "drop-me")

	manager.mu.Lock()
	keepKey := stableSessionKey(repoID, keep)
	dropKey := stableSessionKey(repoID, drop)
	manager.aliveObservations[keepKey] = 7
	manager.aliveObservations[dropKey] = 3
	manager.forgetSessionRuntimeStateLocked(repoID, drop)
	observedKeep, keptPresent := manager.aliveObservations[keepKey]
	_, droppedPresent := manager.aliveObservations[dropKey]
	manager.mu.Unlock()

	require.True(t, keptPresent, "a live session's observation must survive another session's removal")
	require.Equal(t, uint64(7), observedKeep, "the surviving count must be unchanged, not reset")
	require.False(t, droppedPresent, "the removed session's observation must be gone")
}

func sessionTitleForIndex(i int) string {
	return "leak-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
}

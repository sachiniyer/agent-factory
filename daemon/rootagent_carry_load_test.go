package daemon

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The post-restart half of the durable carry (#4400 review round 6). After a
// daemon bounce inside the reap→publish window the record is gone and the
// in-memory park is empty, so reaped-root-carry.json is the only copy of what
// the replacement must restore. These tests drive a fresh Manager — the
// restarted daemon — against a carry file left on disk.

// rootCarryEnsureFailures reads the ensure streak for every candidate; the
// fixture configures one, so the sum is that candidate's count.
func rootCarryEnsureFailures(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, st := range m.rootEnsureStates {
		total += st.consecutiveFailures
	}
	return total
}

func clearRootEnsureBackoff(m *Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, st := range m.rootEnsureStates {
		st.nextAttempt = time.Time{}
	}
}

// TestEnsureRootAgentsFailsWhileTheParkedCarryIsUnreadable is the finding: an
// unreadable carry used to log a warning and fall through to an ambient create,
// whose success then retired the file — a transient read error became a
// permanent loss of the root's account pin. The ensure must fail instead, and
// the next retry after the fault clears must restore the pin.
func TestEnsureRootAgentsFailsWhileTheParkedCarryIsUnreadable(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	_, err = agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	path, err := reapedRootCarryPath(repo.ID)
	require.NoError(t, err)
	// A directory at the carry path makes the read fail with EISDIR for every
	// uid; chmod 000 would not, because CI may run the suite as root.
	require.NoError(t, os.MkdirAll(path, 0o755))

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	require.Empty(t, *seen,
		"an unreadable carry must fail the ensure, not rebuild the root on the ambient identity and retire the carry")
	require.Nil(t, findRootInstance(t, manager, repoPath))
	require.Equal(t, 1, rootCarryEnsureFailures(manager),
		"the refusal is an ensure failure, so the retry-forever cadence and its escalation own it")

	// The fault clears: the carry the reap wrote is readable again.
	require.NoError(t, os.Remove(path))
	payload, err := json.Marshal(reapedRootCarryDisk{Account: "work", Agent: tmux.ProgramClaude})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, payload, 0o600))
	clearRootEnsureBackoff(manager)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 1, "the retry must create the root once the carry reads")
	require.Equal(t, "work", (*seen)[0].Account,
		"the retried create restores the pin the unreadable pass withheld")
	require.NotNil(t, findRootInstance(t, manager, repoPath))
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "the create that consumed the carry retires it")
}

// TestEnsureRootAgentsCreatesAmbientRootWithNoParkedCarry is the canary for the
// stricter guard: "no carry file" is the ordinary first-create case and must
// still create the root at once, on the ambient identity, with no failure
// recorded. Only an unreadable file refuses.
func TestEnsureRootAgentsCreatesAmbientRootWithNoParkedCarry(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	path, err := reapedRootCarryPath(repo.ID)
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "fixture: no carry may exist")

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()

	require.Len(t, *seen, 1, "a missing carry file must not block the first create")
	require.Empty(t, (*seen)[0].Account)
	require.NotNil(t, findRootInstance(t, manager, repoPath))
	require.Zero(t, rootCarryEnsureFailures(manager))
}

// TestReapedRootCarryUnreadableErrorNamesTheFileAndRemedy pins the operator
// half: a fault that never clears keeps the root down on the retry cadence, so
// the logged error must say which file holds the carry and both ways out.
func TestReapedRootCarryUnreadableErrorNamesTheFileAndRemedy(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "deadbeefcafe03"
	path, err := reapedRootCarryPath(repoID)
	require.NoError(t, err)
	cause := os.ErrPermission

	got := reapedRootCarryUnreadableError(repoID, cause)
	require.ErrorIs(t, got, cause, "the load error stays inspectable")
	require.Contains(t, got.Error(), path)
	require.Contains(t, got.Error(), "remove it to start the root on the ambient identity")
}

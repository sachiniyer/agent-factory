package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestEnsureRootAgentsRecreatedRootRecordsRuntimeProgram is the #5066 pin for
// the root agent's in-place reap-and-recreate: every launch must leave the
// resolved runtime command it spawned on the durable record, or doctor's
// "root agent program" check has nothing to compare and — before the fix —
// warned forever with a remedy that could not clear it.
//
// It drives the real LocalBackend against a real (isolated) tmux server: the
// fake backend cannot exercise this invariant because the recording lives at
// the backend's launch boundary, which a fake bypasses.
func TestEnsureRootAgentsRecreatedRootRecordsRuntimeProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	// A cheap verbatim root program: echoes once, then idles. It is not an
	// agent token, so nothing is injected and readiness is the generic
	// any-output heuristic (#1132) at BOTH the create and the recreate.
	const rootProgram = "sh -c 'echo agent-ready; exec sleep 600'"
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{Program: rootProgram}))
	require.NoError(t, err)

	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "the first ensure must create the root")
	t.Cleanup(func() {
		_, _ = manager.KillSession(KillSessionRequest{Title: session.RootSessionTitle, RepoID: repo.ID})
	})
	require.Equal(t, rootProgram, first.RuntimeProgram(),
		"a fresh root create launches a process and must record the command it launched")

	// The #1104 outage class the ensure loop heals: tmux vanished under a
	// healthy daemon, the record reads Dead, and the next pass reaps and
	// re-creates the root in place.
	first.SetStatusForTest(session.Dead)
	manager.ensureRootAgentsAndWait()

	var healed *session.Instance
	waitUntil(t, 10*time.Second, "the dead root to be re-created in place", func() bool {
		healed = findRootInstance(t, manager, repoPath)
		return healed != nil && healed != first && healed.GetStatus() != session.Dead
	})
	require.NotSame(t, first, healed, "the heal must replace the dead instance, not resurrect it")
	require.Equal(t, rootProgram, healed.RuntimeProgram(),
		"the recreate launches a new process and must record the command it launched — "+
			"an empty value here is what made doctor's program check unresolvable (#5066)")

	// The durable projection must carry it too: doctor reads the persisted
	// record, not the live object.
	repoData, err := loadRepoInstanceData(repo.ID)
	require.NoError(t, err)
	var persisted []session.InstanceData
	for _, d := range repoData {
		if d.Title == session.RootSessionTitle {
			persisted = append(persisted, d)
		}
	}
	require.Len(t, persisted, 1, "the heal replaces the root record, not duplicates it")
	require.Equal(t, healed.ID, persisted[0].ID)
	require.Equal(t, rootProgram, persisted[0].RuntimeProgram,
		"the persisted root row must carry the recreated runtime's resolved command")

	// The issue's actual kill shot: the daemon that recorded the launch is
	// restarted, the row is materialized again through LocalBackend.Start's
	// reattach branch, and the pane root still carries the (pid, start-time)
	// identity the launch captured. The claim must survive — before the fix it
	// was retired unconditionally, which is what left a v1.0.298-recreated root
	// with an empty runtime_program on the very next restart (#5066).
	manager.persistInstance(repo.ID, healed)
	restarted, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{Program: rootProgram}))
	require.NoError(t, err)
	require.NoError(t, restarted.RestoreInstances())
	restored := findRootInstance(t, restarted, repoPath)
	require.NotNil(t, restored, "the restarted daemon must materialize the root row")
	require.Equal(t, rootProgram, restored.RuntimeProgram(),
		"a restart reattach that proves the same pane process must keep the recorded command")
}

package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchivedWorkspaceRoundTripsThroughStorage(t *testing.T) {
	data := InstanceData{
		ID: "s1", Title: "t", Path: t.TempDir(), Status: Archived, BackendType: "local",
		Host:              "buildbox",
		Worktree:          GitWorktreeData{RepoPath: "/r", WorktreePath: "/r/wt"},
		ArchivedWorkspace: &ArchivedWorkspaceData{Host: "buildbox", Dir: "/home/u/.af-sessions/s1"},
	}
	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	got := inst.ToInstanceData().ArchivedWorkspace
	require.NotNil(t, got)
	assert.Equal(t, *data.ArchivedWorkspace, *got)
	// The accessor hands out a copy: mutating it must not rewrite the row.
	inst.ArchivedWorkspace().Dir = "elsewhere"
	assert.Equal(t, "/home/u/.af-sessions/s1", inst.ArchivedWorkspace().Dir)
}

// attachStoppableAgent gives inst a live remote client whose agent-server serves
// /v1/agent/stop, counting the calls and refusing them when failStop is set.
func attachStoppableAgent(t *testing.T, inst *Instance, failStop bool) *atomic.Int32 {
	t.Helper()
	stops := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent/stop", func(w http.ResponseWriter, r *http.Request) {
		stops.Add(1)
		if failStop {
			http.Error(w, "stop is not implemented", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]bool{"ok": true}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: srv.URL, Token: "t"}, inst.Title)
	require.NoError(t, err)
	inst.remoteClient = rc
	return stops
}

// #5868: a nil client after a daemon restart is not evidence the agent stopped.
// With no client and nothing to reconnect with, the archive is refused and the
// row is left exactly as it was.
func TestArchivePreserveRefusesWhenStopCannotBeProven(t *testing.T) {
	inst, dir, _ := preservedSessionWithAccount(t, nil)
	inst.remoteClient = nil

	_, err := inst.ArchiveSandboxPreserving()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot confirm its agent has stopped")
	assert.Nil(t, inst.ArchivedWorkspace(), "never recorded as archived over an agent that may still run")
	assert.True(t, inst.started)
	assert.DirExists(t, dir+"/account", "nothing was torn down")
}

// #5868 across a restart: the row is stored, loaded (no client dialled), and the
// archive reconnects from the persisted identity and really stops the agent.
func TestArchivePreserveReattachesAfterRestartThenStops(t *testing.T) {
	live, _, stops := reattachFixtureStops(t, "", false)
	live.ID, live.Path, live.started, live.liveness = "s1", t.TempDir(), true, LiveReady
	srvAddr := live.currentBackend().(*sandboxBackend).provisioner.remoteAddr
	_ = srvAddr
	data := live.ToInstanceData().ForStorage()
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))

	reloaded, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.Nil(t, reloaded.remoteClient, "premise: a load dials nothing")
	// The restarted daemon reaches the agent-server through its tunnel; the test's
	// tunnel is the stand-in server itself.
	reloaded.currentBackend().(*sandboxBackend).provisioner.startTunnelFn =
		live.currentBackend().(*sandboxBackend).provisioner.startTunnelFn

	loc, err := reloaded.ArchiveSandboxPreserving()
	require.NoError(t, err)
	assert.Equal(t, int32(1), stops.Load(), "the agent was stopped, not assumed stopped")
	assert.NotEmpty(t, loc.Dir)
	assert.NotNil(t, reloaded.ArchivedWorkspace())
}

// #5869: a stop the agent-server refuses leaves the session live and usable, with
// the error surfaced, rather than latched closed.
func TestArchivePreserveFailedStopLeavesTheSessionUsable(t *testing.T) {
	inst, dir, _ := preservedSessionWithAccount(t, nil)
	stops := attachStoppableAgent(t, inst, true)

	_, err := inst.ArchiveSandboxPreserving()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to stop session")
	assert.Equal(t, int32(1), stops.Load())
	assert.Nil(t, inst.ArchivedWorkspace())
	assert.NotNil(t, inst.remoteClient, "the live wiring is kept")
	assert.DirExists(t, dir+"/account")

	rs, ok := inst.AgentServer().(*remoteAgentServer)
	require.True(t, ok)
	rs.mu.Lock()
	closed := rs.closed
	rs.mu.Unlock()
	assert.False(t, closed, "the data plane must not stay latched \"being terminated\"")
	_, err = rs.ensureBroker(0)
	assert.NoError(t, err)
}

// #5867: the preserved row's cleanup identity persists and reloads, so a kill or
// prune after a daemon restart reaps what af made.
func TestPreservedArchiveKeepsItsCleanupIdentityAcrossARestart(t *testing.T) {
	inst, _, _ := reattachFixtureStops(t, "", false)
	inst.ID, inst.Path, inst.started, inst.liveness = "s1", t.TempDir(), true, LiveReady
	attachStoppableAgent(t, inst, false)
	loc, err := inst.ArchiveSandboxPreserving()
	require.NoError(t, err)
	inst.mu.Lock()
	inst.liveness = LiveArchived
	inst.mu.Unlock()

	data := inst.ToInstanceData()
	data.Status = Archived
	stored := data.ForStorage()
	require.NotNil(t, stored.RuntimeCleanup, "the handle is written for a preserved workspace")
	require.NotNil(t, stored.RuntimeCleanup.Sandbox)
	assert.Equal(t, loc.Dir, stored.RuntimeCleanup.Sandbox.SessionDir)

	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	reloaded, err := FromInstanceData(back)
	require.NoError(t, err)
	assert.NotNil(t, reloaded.runtimeTeardown, "the reload rebuilds the teardown a kill runs")
	assert.Equal(t, loc.Dir, reloaded.preservedSessionDir())

	// The handle is operator-private: the disk-fallback client view never has it.
	assert.Nil(t, stored.ForClientRead().RuntimeCleanup)
}

// A pushed archive reaped its sandbox, so it keeps no handle.
func TestPushedArchiveStoresNoCleanupIdentity(t *testing.T) {
	inst, _, _ := reattachFixtureStops(t, "", false)
	inst.ID, inst.Path = "s1", t.TempDir()
	inst.archivedWorkspace = &ArchivedWorkspaceData{Pushed: true, Branch: "b"}
	inst.liveness = LiveArchived
	data := inst.ToInstanceData()
	data.Status = Archived
	assert.Nil(t, data.ForStorage().RuntimeCleanup)
}

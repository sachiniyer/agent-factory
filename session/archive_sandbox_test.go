package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackendKindForType pins the persisted-type → runtime-kind mapping the
// re-provision path keys on: the off-box runtimes are re-provisionable
// (#1592 Phase 4 PR6/PR7 — docker/ssh/hook push+re-clone the durable branch);
// only local is rejected (it relocates a worktree instead).
func TestBackendKindForType(t *testing.T) {
	got, err := backendKindForType("docker")
	require.NoError(t, err)
	assert.Equal(t, BackendDocker, got)

	got, err = backendKindForType("ssh")
	require.NoError(t, err)
	assert.Equal(t, BackendSSH, got)

	got, err = backendKindForType("remote")
	require.NoError(t, err)
	assert.Equal(t, BackendHook, got)

	for _, bad := range []string{"local", "", "nope"} {
		if _, err := backendKindForType(bad); err == nil {
			t.Fatalf("backendKindForType(%q): want error", bad)
		}
	}

	assert.True(t, isSandboxBackendType("docker"))
	assert.True(t, isSandboxBackendType("ssh"))
	assert.True(t, isSandboxBackendType("remote"))
	assert.False(t, isSandboxBackendType("local"))
}

// TestNewInertSandboxBackend pins that a loaded sandbox backend classifies the
// session correctly (Type + full remote parity) with no live handle, so archive/
// restore route on it and Kill is a safe no-op (#1592 Phase 4 PR6/PR7).
func TestNewInertSandboxBackend(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want string
	}{{"docker", "docker"}, {"ssh", "ssh"}, {"remote", "remote"}} {
		b := newInertSandboxBackend(tc.typ)
		assert.Equal(t, tc.want, b.Type())
		caps := b.Capabilities()
		assert.Equal(t, WorkspaceRemote, caps.Workspace)
		assert.True(t, caps.Archive, "archive capability")
		assert.True(t, caps.Recover, "recover capability")
		// Nil-reap Kill must not panic (nothing live to tear down).
		require.NoError(t, b.Kill(&Instance{}))
	}
}

// TestFromInstanceData_SandboxBackends pins how a docker/ssh/hook session loads
// from disk (#1592 Phase 4 PR6/PR7): an ARCHIVED record loads inert + Archived
// (restore re-provisions), and a non-archived one loads inert + Lost with
// started=false (so the poll + Lost-restore loop skip it — no dead endpoint
// driven, no infinite backend recursion). Both keep their sandbox
// Type/Capabilities. The hook ("remote") backend now loads the same way, since
// its provision-and-expose migration made it re-provisionable like docker/ssh.
func TestFromInstanceData_SandboxBackends(t *testing.T) {
	for _, typ := range []string{"docker", "ssh", "remote"} {
		t.Run(typ+" archived loads inert + Archived", func(t *testing.T) {
			inst, err := FromInstanceData(InstanceData{
				ID:          "id1",
				Title:       "s",
				Path:        t.TempDir(),
				Branch:      "root/s",
				BackendType: typ,
				Liveness:    LiveArchived,
			})
			require.NoError(t, err)
			assert.Equal(t, typ, inst.GetBackend().Type())
			assert.Equal(t, WorkspaceRemote, inst.Capabilities().Workspace)
			assert.True(t, inst.Capabilities().Archive)
			assert.Equal(t, LiveArchived, inst.GetLiveness())
			assert.False(t, inst.Started())
		})

		t.Run(typ+" live loads inert + Lost, not started", func(t *testing.T) {
			inst, err := FromInstanceData(InstanceData{
				ID:          "id2",
				Title:       "s",
				Path:        t.TempDir(),
				Branch:      "root/s",
				BackendType: typ,
				Liveness:    LiveRunning,
			})
			require.NoError(t, err)
			assert.Equal(t, typ, inst.GetBackend().Type())
			assert.Equal(t, LiveLost, inst.GetLiveness())
			assert.False(t, inst.Started(), "a reloaded sandbox session must not be started (loop skips it)")
		})
	}
}

// TestArchiveSandbox_RejectsNonSandbox pins that ArchiveSandbox is only for
// remote sandbox sessions — a local session (no remote client) is rejected, so
// the daemon's Workspace-kind branch is the only path that reaches it.
func TestArchiveSandbox_RejectsNonSandbox(t *testing.T) {
	i := &Instance{Title: "local-one", backend: &LocalBackend{}}
	if _, err := i.ArchiveSandbox(); err == nil {
		t.Fatal("ArchiveSandbox on a non-sandbox session: want error")
	}
}

// TestReprovisionRemote_RebindsInstance drives the restore re-provision wiring
// without a real sandbox (#1592 Phase 4 PR6): a fakeRuntime swapped into the
// registry returns a fresh backend + authed endpoint + teardown, and
// reprovisionRemote must install all three on the instance (new backend, a
// remoteClient built from the endpoint, and the teardown), replacing the inert
// ones a restore starts from. This is the CI-safe half of the docker/ssh
// round-trip's restore.
func TestReprovisionRemote_RebindsInstance(t *testing.T) {
	freshBackend := &dockerBackend{containerID: "fresh"}
	var toreDown bool
	ep := &AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	fake := fakeRuntime{res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: ep,
		Teardown: func() error { toreDown = true; return nil },
	}}

	// Swap the docker runtime for the fake for the duration of the test.
	prev := runtimeRegistry[BackendDocker]
	runtimeRegistry[BackendDocker] = func() Runtime { return fake }
	defer func() { runtimeRegistry[BackendDocker] = prev }()

	// Start from an inert, archived-style docker instance (no live wiring).
	i := &Instance{Title: "s", Path: t.TempDir(), Branch: "root/s", backend: newInertSandboxBackend("docker")}

	require.NoError(t, i.reprovisionRemote())

	i.mu.RLock()
	gotBackend := i.backend
	gotClient := i.remoteClient
	gotTeardown := i.runtimeTeardown
	i.mu.RUnlock()
	assert.Same(t, freshBackend, gotBackend, "backend rebound to the fresh sandbox")
	require.NotNil(t, gotClient, "remote agent-server client built from the new endpoint")
	require.NotNil(t, gotTeardown, "teardown rebound to the fresh sandbox")
	// The cached agent-server is discarded so AgentServer() rebuilds against the
	// new client (cache + fields share i.mu since #1729).
	i.mu.RLock()
	assert.Nil(t, i.agentSrv)
	i.mu.RUnlock()
	assert.False(t, toreDown, "a successful bind does not tear the new sandbox down")
}

// TestResetRemoteRuntime clears the live wiring but keeps the backend (which
// carries the sandbox Type/Capabilities for load + restore).
func TestResetRemoteRuntime(t *testing.T) {
	i := &Instance{
		Title:           "s",
		backend:         &dockerBackend{containerID: "c"},
		remoteClient:    &remoteAgentClient{title: "s"},
		runtimeTeardown: func() error { return nil },
		agentSrv:        &remoteAgentServer{},
	}
	i.resetRemoteRuntime()
	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Nil(t, i.remoteClient)
	assert.Nil(t, i.runtimeTeardown)
	assert.Nil(t, i.agentSrv)
	assert.Equal(t, "docker", i.backend.Type(), "backend is kept for restore classification")
}

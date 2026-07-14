package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingStartBackend stands in for a freshly re-provisioned sandbox whose agent
// Start fails: it embeds FakeBackend for the full interface and overrides Start to
// error immediately, plus Type() so the instance stays classified as its sandbox
// runtime. Used to drive the #1726 Start-failure leak guard on restore/recover.
type failingStartBackend struct {
	*FakeBackend
	typ string
}

func (b *failingStartBackend) Start(*Instance, bool) error { return fmt.Errorf("start failed") }
func (b *failingStartBackend) Type() string                { return b.typ }

// countingRuntime is a Runtime that provisions a failing-Start sandbox and tracks
// how many sandboxes are live at once, so a test can prove the Start-failure guard
// reaps each sandbox before a retry provisions the next — never stacking two
// (#1726). Its Teardown decrements the live count; Provision records the peak.
type countingRuntime struct {
	endpoint   *AgentServerEndpoint
	typ        string
	provisions int
	live       int
	maxLive    int
}

func (r *countingRuntime) Provision(ProvisionSpec) (ProvisionResult, error) {
	r.provisions++
	r.live++
	if r.live > r.maxLive {
		r.maxLive = r.live
	}
	return ProvisionResult{
		Backend:  &failingStartBackend{FakeBackend: NewFakeBackend(), typ: r.typ},
		Endpoint: r.endpoint,
		Teardown: func() error { r.live--; return nil },
	}, nil
}

// assertRemoteRuntimeReset pins that the Start-failure teardown left the instance
// unbound (no half-bound remote wiring), so a retry re-provisions from clean state.
func assertRemoteRuntimeReset(t *testing.T, i *Instance) {
	t.Helper()
	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Nil(t, i.remoteClient, "remoteClient cleared after Start-failure teardown")
	assert.Nil(t, i.runtimeTeardown, "runtimeTeardown cleared after Start-failure teardown")
	assert.Nil(t, i.agentSrv, "cached agent-server cleared after Start-failure teardown")
}

// TestRecoverSandbox_TeardownOnStartFailure proves the #1726 fix in recoverSandbox:
// when reprovisionRemote succeeds but Start fails, the freshly provisioned sandbox
// is torn down (its Teardown IS called) and the remote runtime state is reset, so
// the container/remote is reclaimed rather than leaked.
func TestRecoverSandbox_TeardownOnStartFailure(t *testing.T) {
	var tornDown bool
	ep := &AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	fake := fakeRuntime{res: ProvisionResult{
		Backend:  &failingStartBackend{FakeBackend: NewFakeBackend(), typ: "docker"},
		Endpoint: ep,
		Teardown: func() error { tornDown = true; return nil },
	}}

	prev := runtimeRegistry[BackendDocker]
	runtimeRegistry[BackendDocker] = func() Runtime { return fake }
	defer func() { runtimeRegistry[BackendDocker] = prev }()

	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		liveness: LiveArchived,
	}

	err := recoverSandbox(i)
	require.Error(t, err, "Start must fail for this test")
	assert.Contains(t, err.Error(), "start failed", "error from failingStartBackend")
	assert.True(t, tornDown, "Teardown must be called when Start fails after reprovisionRemote succeeds")
	assertRemoteRuntimeReset(t, i)
}

// TestRestoreSandbox_TeardownOnStartFailure proves the same #1726 fix in
// RestoreSandbox (the archive-restore mechanic).
func TestRestoreSandbox_TeardownOnStartFailure(t *testing.T) {
	var tornDown bool
	ep := &AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint}
	fake := fakeRuntime{res: ProvisionResult{
		Backend:  &failingStartBackend{FakeBackend: NewFakeBackend(), typ: "docker"},
		Endpoint: ep,
		Teardown: func() error { tornDown = true; return nil },
	}}

	prev := runtimeRegistry[BackendDocker]
	runtimeRegistry[BackendDocker] = func() Runtime { return fake }
	defer func() { runtimeRegistry[BackendDocker] = prev }()

	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		liveness: LiveArchived,
	}

	err := i.RestoreSandbox()
	require.Error(t, err, "Start must fail for this test")
	assert.Contains(t, err.Error(), "start failed", "error from failingStartBackend")
	assert.True(t, tornDown, "Teardown must be called when Start fails after reprovisionRemote succeeds")
	assertRemoteRuntimeReset(t, i)
}

// TestRecoverSandbox_RetryDoesNotStackSandboxes proves the retry half of #1726:
// after a Start-failure reaps the first sandbox and resets the wiring, a second
// recover attempt provisions a fresh sandbox WITHOUT the first still running — the
// Lost-restore loop never stacks two sandboxes. The countingRuntime asserts at most
// one sandbox is ever live at once across both attempts.
func TestRecoverSandbox_RetryDoesNotStackSandboxes(t *testing.T) {
	rt := &countingRuntime{
		endpoint: &AgentServerEndpoint{URL: "wss://127.0.0.1:9", Token: "tok", Fingerprint: validFingerprint},
		typ:      "docker",
	}
	prev := runtimeRegistry[BackendDocker]
	runtimeRegistry[BackendDocker] = func() Runtime { return rt }
	defer func() { runtimeRegistry[BackendDocker] = prev }()

	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		liveness: LiveArchived,
	}

	require.Error(t, recoverSandbox(i), "first attempt: Start fails")
	assertRemoteRuntimeReset(t, i)
	require.Error(t, recoverSandbox(i), "retry: Start fails again")

	assert.Equal(t, 2, rt.provisions, "each attempt provisions a fresh sandbox")
	assert.Equal(t, 0, rt.live, "no sandbox left running after both attempts")
	assert.Equal(t, 1, rt.maxLive, "the first sandbox is reaped before the retry provisions the second — never two at once")
}

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

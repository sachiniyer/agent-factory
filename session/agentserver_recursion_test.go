package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recursiveRemoteBackend reproduces the load-bearing shape of the real sandbox
// backends (docker/ssh/hook) for the #2005 regression: it reports a WorkspaceRemote
// workspace and its HasUpdated delegates STRAIGHT BACK to i.AgentServer().Snapshot(),
// exactly like remoteAgentBackend does. A depth counter with a hard cap stands in
// for the kernel stack limit, so the pre-fix infinite recursion is observed as a
// bounded depth spike this test can assert on, instead of crashing the whole test
// binary with a real (unrecoverable) stack overflow. Start fails, so it drives
// recoverSandbox down its teardown path — the exact production trigger.
type recursiveRemoteBackend struct {
	*FakeBackend
	typ      string
	depth    int
	maxDepth int
}

func (b *recursiveRemoteBackend) Type() string { return b.typ }

func (b *recursiveRemoteBackend) Start(*Instance, bool) error { return fmt.Errorf("start failed") }

func (b *recursiveRemoteBackend) Capabilities() Capabilities {
	c := b.FakeBackend.Capabilities()
	c.Workspace = WorkspaceRemote
	return c
}

// HasUpdated mirrors remoteAgentBackend.HasUpdated: delegate to the agent-server.
// With the #2005 bug AgentServer() hands back a localAgentServer whose Snapshot()
// calls right back here — unbounded. The cap keeps the binary alive so the test can
// ASSERT the recursion rather than die from it.
func (b *recursiveRemoteBackend) HasUpdated(i *Instance) (bool, bool, string) {
	b.depth++
	if b.depth > b.maxDepth {
		return false, false, ""
	}
	obs, _ := i.AgentServer().Snapshot()
	return obs.Updated, obs.HasPrompt, obs.Content
}

// TestAgentServer_TornDownRemoteRuntime_DoesNotRecurse is the #2005 regression.
//
// It drives the real failed-lost-recovery path: reprovisionRemote binds a fresh
// remote backend + client, Start fails, and teardownAfterStartFailure clears the
// client — leaving the instance started+Lost with a WorkspaceRemote backend still
// bound but remoteClient nil. On the next poll tick, AgentServer().Snapshot() (the
// probe refreshInstanceStatus runs) must NOT recurse into a localAgentServer that
// delegates back through the remote backend forever.
func TestAgentServer_TornDownRemoteRuntime_DoesNotRecurse(t *testing.T) {
	probe := &recursiveRemoteBackend{FakeBackend: NewFakeBackend(), typ: "docker", maxDepth: 64}
	fake := fakeRuntime{res: ProvisionResult{
		Backend:  probe,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error { return nil },
	}}

	prev := runtimeRegistry[BackendDocker]
	runtimeRegistry[BackendDocker] = func() Runtime { return fake }
	defer func() { runtimeRegistry[BackendDocker] = prev }()

	// A running remote session that went Lost keeps started=true (that is why the
	// poll and the restore loop still touch it). newInertSandboxBackend gives the
	// pre-recovery inert docker backend; reprovisionRemote swaps in `probe`.
	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		started:  true,
		liveness: LiveLost,
	}

	require.Error(t, recoverSandbox(i), "Start must fail so recovery leaves the torn-down state")

	// Post-failed-recovery invariant: the remote wiring is cleared, but the session
	// stays started+Lost so the #1128 auto-restore loop keeps retrying it. (Setting
	// started=false here would strand auto-recovery — that is the wrong fix.)
	i.mu.RLock()
	require.Nil(t, i.remoteClient, "remoteClient cleared by the failed-recovery teardown")
	require.True(t, i.started, "started stays true so auto-recovery keeps retrying (#1128)")
	require.Equal(t, LiveLost, i.liveness, "session stays Lost after a failed recovery")
	require.Equal(t, WorkspaceRemote, i.backend.Capabilities().Workspace, "a remote backend is still bound")
	i.mu.RUnlock()

	// The poll tick's probe. Pre-fix: AgentServer() returns a localAgentServer whose
	// Snapshot() delegates to backend.HasUpdated() → AgentServer().Snapshot() → …
	// unbounded, driving probe.depth to the cap. Post-fix: AgentServer() returns a
	// deadRemoteAgentServer whose Snapshot() errors WITHOUT ever touching the backend.
	obs, err := i.AgentServer().Snapshot()

	assert.LessOrEqual(t, probe.depth, 1,
		"AgentServer() must not recurse through the remote backend (#2005); depth=%d means it did", probe.depth)
	assert.Error(t, err, "a torn-down remote runtime reports its sandbox as gone, not a healthy snapshot")
	assert.Equal(t, Observation{}, obs, "no observation when there is no live agent-server")

	// The instance resolves to a DEFINITE, coherent state — a real Lost, not a
	// spinning or corrupted one — and stays started so the restore loop can retry.
	i.mu.RLock()
	assert.Equal(t, LiveLost, i.liveness, "poll leaves the session in a definite Lost state")
	assert.True(t, i.started, "poll does not clear started (auto-recovery stays eligible)")
	i.mu.RUnlock()
}

// TestAgentServer_TornDownRemoteRuntime_KillDeletesCleanly proves a user can still
// kill a session stuck in the torn-down remote state: instance.Kill() routes to
// the dead server, which is a no-op success (there is nothing live to reap), so the
// daemon's KillSession proceeds to delete the record rather than keeping it for a
// doomed retry.
func TestAgentServer_TornDownRemoteRuntime_KillDeletesCleanly(t *testing.T) {
	i := &Instance{
		Title:    "s",
		backend:  newInertSandboxBackend("docker"),
		started:  true,
		liveness: LiveLost,
	}
	// remoteClient is nil + a remote backend => the dead server.
	_, ok := i.AgentServer().(*deadRemoteAgentServer)
	require.True(t, ok, "a torn-down remote instance must get the dead server, never a localAgentServer")

	assert.NoError(t, i.Kill(), "killing a torn-down remote session must succeed so its record deletes")
}

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SetRuntimeTeardownForTest exists so a daemon fixture can observe the PHYSICAL
// sandbox reap instead of the /v1/agent/kill message (#3042). That only works if
// the reap it installs is the one AgentServer().Kill() will actually run — and
// remoteAgentServer captures teardown BY VALUE when the agentSrv cache is built:
//
//	i.agentSrv = &remoteAgentServer{rc: i.remoteClient, inst: i, teardown: i.runtimeTeardown}
//
// So a teardown installed while that cache is already warm is never invoked. The
// three production writers of runtimeTeardown (bindProvisionResult,
// retainProvisionResultCleanup, resetRemoteRuntime) all clear i.agentSrv in the
// SAME i.mu section for this reason, citing #1729; the test helper did not.
//
// The consequence is the failure mode #3042 is about, one level in: the fixture
// looks like it observes the reap, the reap counter stays at zero, and a test
// asserting "the sandbox was released" fails for a reason that looks like a
// product bug — while a test asserting "nothing was reaped" PASSES no matter what
// production does. A cache-ordering accident would decide which.
//
// Any test that polls, previews or probes before archiving warms that cache, so
// this cannot be left to call order across 30-odd fixture call sites.
func TestSetRuntimeTeardownForTest_IsInstalledOnAWarmAgentServerCache(t *testing.T) {
	i := &Instance{Title: "cached-endpoint"}
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: "http://127.0.0.1:1", Token: "t"}, i.Title)
	require.NoError(t, err)
	i.remoteClient = rc

	// Warm the cache BEFORE installing the reap — what a poll tick does.
	first := i.AgentServer()
	require.IsType(t, &remoteAgentServer{}, first,
		"precondition: a session with a remote client gets the remote agent-server")

	reaps := 0
	SetRuntimeTeardownForTest(i, func() error { reaps++; return nil })

	// Kill's REST call fails (nothing listens on port 1); the teardown must run
	// anyway — that is remoteAgentServer.Kill's documented contract, "the container
	// must not leak because its agent-server was already down".
	_ = i.AgentServer().Kill()

	assert.Equal(t, 1, reaps,
		"the installed reap never ran: AgentServer() returned a cache built before it, so a daemon "+
			"fixture would observe zero reaps regardless of what production did — the #3042 blind spot "+
			"reproduced inside the helper meant to fix it")
}

// The same property stated as the invariant rather than the symptom: the helper
// must leave the cache in whatever state makes it re-derive from the fields. This
// one fails even if Kill's contract changes, and it is the assertion to keep.
func TestSetRuntimeTeardownForTest_InvalidatesTheDerivedCache(t *testing.T) {
	i := &Instance{Title: "invalidate"}
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: "http://127.0.0.1:1", Token: "t"}, i.Title)
	require.NoError(t, err)
	i.remoteClient = rc

	before := i.AgentServer()
	SetRuntimeTeardownForTest(i, func() error { return nil })
	after := i.AgentServer()

	assert.NotSame(t, before, after,
		"installing a runtime teardown must invalidate the agentSrv cache derived from it, exactly as "+
			"bindProvisionResult/resetRemoteRuntime do under the same lock (#1729) — otherwise the field "+
			"and the server that reads it disagree")
	require.NotNil(t, after.(*remoteAgentServer).teardown,
		"and the rebuilt server must carry the reap, not a nil handle")
}

// The clearing direction, which the daemon's refusal tests depend on: a fixture
// that installs a reap and then removes it must not leave a cached server still
// holding the old one, or "nothing was reaped" would be unobservable in the
// opposite direction.
func TestSetRuntimeTeardownForTest_ClearingIsAlsoVisible(t *testing.T) {
	i := &Instance{Title: "clear"}
	rc, err := newRemoteAgentClient(AgentServerEndpoint{URL: "http://127.0.0.1:1", Token: "t"}, i.Title)
	require.NoError(t, err)
	i.remoteClient = rc

	reaps := 0
	SetRuntimeTeardownForTest(i, func() error { reaps++; return nil })
	_ = i.AgentServer() // warm the cache with the reap installed
	SetRuntimeTeardownForTest(i, nil)
	_ = i.AgentServer().Kill()

	assert.Zero(t, reaps, "a cleared teardown must not keep running from a stale cached server")
}

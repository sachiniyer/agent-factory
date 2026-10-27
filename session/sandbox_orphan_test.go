package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3480: a create that provisioned a sandbox and then could NOT confirm it torn
// down must hand the caller something that can still reach it. Before this, both
// create-path exits named the possible orphan only in an error string — no
// handle, no row, nothing any later operation could reap.

// assertOrphanIsReapable is the whole contract in one place: the failed create
// carries a cleanup-only record whose durable teardown identity survives the
// storage boundary AND rebuilds into a live teardown on the other side. The
// round-trip is the load-bearing half — an in-memory handle dies with the daemon,
// and the daemon restart is exactly when an operator goes looking for the leak.
func assertOrphanIsReapable(t *testing.T, err error, wantBackend Backend, wantID string) {
	t.Helper()
	var orphan *SandboxOrphanError
	require.ErrorAs(t, err, &orphan, "an unconfirmed teardown must carry a reapable orphan, not just error text")
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown, "wrapping the orphan must not hide the unknown-state sentinel")

	rec := orphan.OrphanRecord()
	require.NotNil(t, rec, "the orphan error must carry a record")

	// BORN tombstoned, asserted before anything here marks it. The advertised
	// fail-safe is that a record cannot escape in Lost-and-not-tombstoned shape —
	// the one the Lost-restore loop would pick up and "recover" into a second
	// sandbox beside the orphan. Marking it in this helper first would mask the
	// removal of the production call entirely.
	require.True(t, rec.UserKilled(), "the orphan record must be born tombstoned, not rely on its caller")

	// The create announced a provisional row under this id and every later
	// projection upserts onto it; a retained row that minted its own would reach
	// clients as a second identity beside the pending one.
	require.Equal(t, wantID, rec.ID, "the orphan must keep the identity the create was announced under")
	rec.mu.RLock()
	assert.Same(t, wantBackend, rec.backend, "the record must name the sandbox that may still be running")
	assert.NotNil(t, rec.runtimeTeardown, "the record must carry the handle that can reap it")
	assert.Nil(t, rec.remoteClient, "the create failed; nothing about this sandbox is usable")
	assert.True(t, rec.runtimeCleanupStateUnknown, "the unknown outcome must be recorded, not assumed settled")
	rec.mu.RUnlock()

	// The same storage round-trip the restore-path orphan tests use.
	assertUnknownCleanupSurvivesRestart(t, rec)

	// And as the daemon actually persists it — keepFailedCreate tombstones the row
	// so SaveInstances keeps it and the poll routes it to finishUserKill.
	rec.MarkUserKilled()
	stored := rec.ToInstanceData().ForStorage()
	require.True(t, stored.UserKilled, "the tombstone is what makes the row survive a wholesale checkpoint")
	require.NotNil(t, stored.RuntimeCleanup, "a tombstoned orphan must keep its teardown identity")
}

// The revalidation exit under W3.1 (#5571): the sandbox came up and the
// credential minted for it was invalidated while it was being provisioned — and
// the session DEGRADES instead of being discarded. A callback-less sandbox is
// still a working session, so nothing is reaped and nothing fails: the reason
// rides ProvisionResult.CallbackUnavailable onto the durable record instead.
func TestDefaultBackendFactory_RevalidationFailureDegradesAndKeepsTheSandbox(t *testing.T) {
	provisioned := &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/orphan",
	}}
	teardowns := 0
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  provisioned,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error { teardowns++; return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown) },
	}}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	creds := &fakeCreds{invalid: errors.New("the listener moved while the sandbox was being provisioned")}
	res, err := defaultBackendFactoryForKind(InstanceOptions{
		ID:                 "announced-id",
		Title:              "s",
		Program:            "claude",
		SandboxCredentials: creds,
	}, t.TempDir(), BackendSandbox)

	require.NoError(t, err, "a dead callback must not fail a create whose sandbox is otherwise fine")
	assert.Equal(t, "the listener moved while the sandbox was being provisioned", res.CallbackUnavailable,
		"the degraded reason is the revalidation's own text — surfaces render it verbatim")
	assert.Equal(t, 0, teardowns,
		"the sandbox is KEPT — reaping it would destroy a session that merely lost a capability")
	assert.Same(t, provisioned, res.Backend)
	assert.NotNil(t, res.Teardown, "the teardown handle stays with the session that now owns the sandbox")
	assert.NotZero(t, creds.revokes, "the dead grant is still revoked for tidiness")
}

// The client-build exit: the sandbox is up but its endpoint could not be wired,
// and the reap could not confirm the sandbox is gone.
func TestNewInstance_ClientBuildFailureWithUnknownCleanupYieldsAReapableOrphan(t *testing.T) {
	provisioned := &dockerBackend{
		containerID: "orphan",
		cleanup:     &DockerRuntimeCleanupData{ContainerID: "orphan", EngineID: "engine-id"},
	}
	previousFactory := backendFactory
	backendFactory = func(InstanceOptions, string, BackendKind) (ProvisionResult, error) {
		return ProvisionResult{
			Backend:  provisioned,
			Endpoint: &AgentServerEndpoint{URL: "://invalid", Token: "tok"},
			Teardown: func() error { return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown) },
		}, nil
	}
	defer func() { backendFactory = previousFactory }()

	_, err := NewInstance(InstanceOptions{ID: "announced-id", Title: "s", Path: t.TempDir(), Program: "claude"})
	require.ErrorContains(t, err, "failed to build remote agent-server client")
	assertOrphanIsReapable(t, err, provisioned, "announced-id")
}

// A KNOWN teardown outcome means the sandbox IS gone, so there is nothing to
// reap and no orphan to record: NewInstance SURFACES a known cleanup error (a
// create failing for a reason it can name should say so) and it must not
// manufacture an orphan for it. (The revalidation half of this test was
// retired by #5571 W3.1 — a dead callback now degrades and KEEPS the sandbox,
// so there is no teardown on that path for a known outcome to be reported
// about.)
func TestCreateOrphan_KnownCleanupOutcomeIsNotAnOrphan(t *testing.T) {
	known := errors.New("runtime answered with a cleanup failure")

	previousFactory := backendFactory
	backendFactory = func(InstanceOptions, string, BackendKind) (ProvisionResult, error) {
		return ProvisionResult{
			Backend:  newInertSandboxBackend("docker"),
			Endpoint: &AgentServerEndpoint{URL: "://invalid", Token: "tok"},
			Teardown: func() error { return known },
		}, nil
	}
	defer func() { backendFactory = previousFactory }()

	_, err := NewInstance(InstanceOptions{Title: "s", Path: t.TempDir(), Program: "claude"})
	var orphan *SandboxOrphanError
	require.False(t, errors.As(err, &orphan), "a confirmed teardown must not manufacture an orphan record")
	require.ErrorIs(t, err, known, "the known cleanup failure must still surface, as it deliberately does today")
}

// A revalidation cause that happens to carry ErrWorkspaceStateUnknown is a
// REASON STRING now, not an error anyone classifies (#5571 W3.1): nothing calls
// TeardownStateUnknown on it because nothing reaps — the sandbox is kept and
// the text lands on the durable record verbatim. The misclassified-orphan bug
// this test used to guard is removed at the root, not preserved and worked
// around.
func TestCreateOrphan_RevalidationCauseCarryingSentinelStillDegrades(t *testing.T) {
	teardowns := 0
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SSHCommand: "ssh h", SessionDir: "/d"}},
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error { teardowns++; return nil },
	}}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	// The revalidation failure carries the teardown sentinel itself — under the
	// old contract this combination reaped the sandbox AND risked an orphan
	// record on misclassification; under W3.1 it degrades plainly.
	invalidated := fmt.Errorf("%w: the listener probe timed out", ErrWorkspaceStateUnknown)
	res, err := defaultBackendFactoryForKind(InstanceOptions{
		ID:                 "announced-id",
		Title:              "s",
		Program:            "claude",
		SandboxCredentials: &fakeCreds{invalid: invalidated},
	}, t.TempDir(), BackendSandbox)

	require.NoError(t, err)
	assert.Equal(t, invalidated.Error(), res.CallbackUnavailable,
		"the reason carries the cause's full text — sentinel included — for the surfaces to render")
	assert.Equal(t, 0, teardowns, "the sandbox is kept; only a real teardown classifies cleanup state")
}

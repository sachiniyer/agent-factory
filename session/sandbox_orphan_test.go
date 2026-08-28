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
func assertOrphanIsReapable(t *testing.T, err error, wantBackend Backend) {
	t.Helper()
	var orphan *SandboxOrphanError
	require.ErrorAs(t, err, &orphan, "an unconfirmed teardown must carry a reapable orphan, not just error text")
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown, "wrapping the orphan must not hide the unknown-state sentinel")

	rec := orphan.OrphanRecord()
	require.NotNil(t, rec, "the orphan error must carry a record")
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

// The revalidation exit: the sandbox came up, the credential minted for it was
// invalidated while it was being provisioned, and the reap that follows could not
// establish whether the sandbox is gone.
func TestDefaultBackendFactory_RevalidationFailureWithUnknownCleanupYieldsAReapableOrphan(t *testing.T) {
	provisioned := &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/orphan",
	}}
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  provisioned,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error { return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown) },
	}}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	creds := &fakeCreds{invalid: errors.New("the listener moved while the sandbox was being provisioned")}
	_, err := defaultBackendFactoryForKind(InstanceOptions{
		Title:              "s",
		Program:            "claude",
		SandboxCredentials: creds,
	}, t.TempDir(), BackendSandbox)

	require.Error(t, err)
	assertOrphanIsReapable(t, err, provisioned)
	assert.NotZero(t, creds.revokes, "the credential minted for an unusable sandbox must not outlive it")
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

	_, err := NewInstance(InstanceOptions{Title: "s", Path: t.TempDir(), Program: "claude"})
	require.ErrorContains(t, err, "failed to build remote agent-server client")
	assertOrphanIsReapable(t, err, provisioned)
}

// A KNOWN teardown outcome means the sandbox IS gone, so there is nothing to
// reap and no orphan to record. This guards the deliberate difference between the
// two exits rather than normalizing it away: NewInstance SURFACES a known cleanup
// error (a create failing for a reason it can name should say so) while
// discardUnusableSandbox logs it. Neither may start producing an orphan.
func TestCreateOrphan_KnownCleanupOutcomeIsNotAnOrphan(t *testing.T) {
	known := errors.New("runtime answered with a cleanup failure")

	t.Run("client build", func(t *testing.T) {
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
	})

	t.Run("revalidation", func(t *testing.T) {
		rt := fakeRuntime{res: ProvisionResult{
			Backend:  &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{SSHCommand: "ssh h", SessionDir: "/d"}},
			Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
			Teardown: func() error { return known },
		}}
		restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
		defer restoreRuntime()

		_, err := defaultBackendFactoryForKind(InstanceOptions{
			Title:              "s",
			Program:            "claude",
			SandboxCredentials: &fakeCreds{invalid: errors.New("invalidated")},
		}, t.TempDir(), BackendSandbox)
		var orphan *SandboxOrphanError
		require.Error(t, err)
		require.False(t, errors.As(err, &orphan), "a confirmed teardown must not manufacture an orphan record")
	})
}

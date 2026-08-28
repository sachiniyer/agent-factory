package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type orderedReprovisionRuntime struct {
	events *[]string
	res    ProvisionResult
}

type cleanupFailingStartBackend struct {
	*dockerBackend
}

func (b *cleanupFailingStartBackend) Start(*Instance, bool) error {
	return fmt.Errorf("start failed")
}

func (r *orderedReprovisionRuntime) Provision(ProvisionSpec) (ProvisionResult, error) {
	*r.events = append(*r.events, "provision")
	return r.res, nil
}

// TestReprovisionRemote_ReapsPreviousRuntimeBeforeProvision proves replacement
// never discards the previous sandbox's teardown handle. A completed teardown
// error follows the established best-effort contract, but is still attempted
// before the replacement is provisioned.
func TestReprovisionRemote_ReapsPreviousRuntimeBeforeProvision(t *testing.T) {
	for _, tc := range []struct {
		name        string
		teardownErr error
	}{
		{name: "success"},
		{name: "known failure", teardownErr: errors.New("runtime answered with a cleanup failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			freshBackend := &dockerBackend{containerID: "fresh"}
			rt := &orderedReprovisionRuntime{
				events: &events,
				res: ProvisionResult{
					Backend:  freshBackend,
					Teardown: func() error { return nil },
				},
			}
			restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
			defer restoreRuntime()

			i := &Instance{
				Title:   "s",
				Path:    t.TempDir(),
				Branch:  "root/s",
				backend: newInertSandboxBackend("docker"),
				runtimeTeardown: func() error {
					events = append(events, "old teardown")
					return tc.teardownErr
				},
			}

			require.NoError(t, i.reprovisionRemote())
			assert.Equal(t, []string{"old teardown", "provision"}, events)
			assert.Same(t, freshBackend, i.GetBackend())
		})
	}
}

// TestReprovisionRemote_RetainsPreviousRuntimeWhenTeardownUnknown proves an
// unanswerable cleanup is not treated as success or as a completed failure. No
// replacement is provisioned, and the old wiring remains available to retry.
func TestReprovisionRemote_RetainsPreviousRuntimeWhenTeardownUnknown(t *testing.T) {
	var events []string
	rt := &orderedReprovisionRuntime{events: &events, res: ProvisionResult{Backend: &dockerBackend{containerID: "fresh"}}}
	restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
	defer restoreRuntime()

	oldBackend := &dockerBackend{
		containerID: "old",
		cleanup: &DockerRuntimeCleanupData{
			ContainerID: "old",
			EngineID:    "engine-id",
		},
	}
	oldClient := &remoteAgentClient{title: "old"}
	oldTeardown := func() error {
		return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)
	}
	i := &Instance{
		Title:           "s",
		Path:            t.TempDir(),
		Branch:          "root/s",
		backend:         oldBackend,
		remoteClient:    oldClient,
		runtimeTeardown: oldTeardown,
	}

	err := i.reprovisionRemote()
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown)
	assert.Empty(t, events, "unknown old-sandbox cleanup must stop before provisioning")

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Same(t, oldBackend, i.backend)
	assert.Same(t, oldClient, i.remoteClient)
	assert.NotNil(t, i.runtimeTeardown, "the retryable cleanup handle must remain installed")
	assertUnknownCleanupSurvivesRestart(t, i)
}

// TestRecoverSandbox_RetainsRuntimeWhenStartFailureCleanupUnknown covers the
// adjacent replacement path: if the freshly provisioned sandbox cannot be
// confirmed reaped after Start fails, retain its wiring instead of orphaning it.
func TestRecoverSandbox_RetainsRuntimeWhenStartFailureCleanupUnknown(t *testing.T) {
	freshBackend := &cleanupFailingStartBackend{dockerBackend: &dockerBackend{
		containerID: "fresh",
		cleanup: &DockerRuntimeCleanupData{
			ContainerID: "fresh",
			EngineID:    "engine-id",
		},
	}}
	ep := &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"}
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: ep,
		Teardown: func() error {
			return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)
		},
	}}
	restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		Title:    "s",
		Path:     t.TempDir(),
		Branch:   "root/s",
		backend:  newInertSandboxBackend("docker"),
		liveness: LiveLost,
	}

	err := recoverSandbox(i)
	require.ErrorContains(t, err, "start failed")
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown)

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Same(t, freshBackend, i.backend)
	assert.NotNil(t, i.remoteClient)
	assert.NotNil(t, i.runtimeTeardown, "unknown cleanup must keep the handle available for retry")
	assertUnknownCleanupSurvivesRestart(t, i)
}

// TestReprovisionRemote_RetainsNewRuntimeWhenBindFailureCleanupUnknown covers
// the remaining replacement failure window: provisioning succeeded, endpoint
// validation failed, and cleanup could not establish whether the new sandbox
// was reaped. Its handle must become the instance's next retry target.
func TestReprovisionRemote_RetainsNewRuntimeWhenBindFailureCleanupUnknown(t *testing.T) {
	freshBackend := &dockerBackend{
		containerID: "fresh",
		cleanup: &DockerRuntimeCleanupData{
			ContainerID: "fresh",
			EngineID:    "engine-id",
		},
	}
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: &AgentServerEndpoint{URL: "://invalid", Token: "tok"},
		Teardown: func() error {
			return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)
		},
	}}
	restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		Title:   "s",
		Path:    t.TempDir(),
		Branch:  "root/s",
		backend: newInertSandboxBackend("docker"),
	}

	err := i.reprovisionRemote()
	require.ErrorContains(t, err, "failed to bind re-provisioned sandbox")
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown)

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Same(t, freshBackend, i.backend)
	assert.Nil(t, i.remoteClient)
	assert.NotNil(t, i.runtimeTeardown, "unknown cleanup must keep the new sandbox's handle")
	assertUnknownCleanupSurvivesRestart(t, i)
}

// assertUnknownCleanupSurvivesRestart exercises the production storage
// round-trip used by both automatic and manual restore failure paths. A cleanup
// outcome that could not be determined must keep its exact teardown identity;
// otherwise the next daemon provisions over a sandbox that may still exist.
func assertUnknownCleanupSurvivesRestart(t *testing.T, i *Instance) {
	t.Helper()
	stored := i.ToInstanceData().ForStorage()
	require.True(t, stored.RuntimeCleanupStateUnknown, "unknown cleanup lost its durable state marker")
	require.NotNil(t, stored.RuntimeCleanup, "unknown cleanup lost its teardown identity at the storage boundary")
	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown, "unknown cleanup restored without a retryable teardown")
}

// TestReprovisionRemote_RetainsNewRuntimeWhenRevalidationFailureCleanupUnknown
// covers the third post-provision failure window, and the one that drifted
// (#3475): the replacement came up, but the credential minted for it was
// invalidated while it was being provisioned, and the reap that follows could
// not establish whether the sandbox is gone.
//
// Same condition as the bind-failure sibling above, so it must reach the same
// answer — an unconfirmed teardown never drops the only handle on a sandbox that
// may still be running.
func TestReprovisionRemote_RetainsNewRuntimeWhenRevalidationFailureCleanupUnknown(t *testing.T) {
	creds := &fakeCreds{invalid: errors.New("the listener moved while the sandbox was being provisioned")}
	freshBackend := &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/fresh",
	}}
	teardowns := 0
	rt := fakeRuntime{res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error {
			teardowns++
			return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)
		},
	}}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		Title:   "s",
		Path:    t.TempDir(),
		Branch:  "root/s",
		backend: newInertSandboxBackend("sandbox"),
	}
	i.SetSandboxCredentials(creds)

	err := i.reprovisionRemote()
	require.ErrorContains(t, err, "callback credential was invalidated")
	require.ErrorIs(t, err, ErrWorkspaceStateUnknown)
	assert.Equal(t, 1, teardowns, "an unusable replacement must still be reaped")
	assert.NotZero(t, creds.revokes, "the credential minted for an unusable sandbox must not outlive it")

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Same(t, freshBackend, i.backend, "the identity of the sandbox that may still be running must be retained")
	assert.Nil(t, i.remoteClient, "the replacement is unusable; only its cleanup handle is kept")
	assert.NotNil(t, i.runtimeTeardown, "unknown cleanup must keep the new sandbox's handle for retry")
	assert.True(t, i.runtimeCleanupStateUnknown, "the unknown outcome must be recorded, not assumed settled")
	assertUnknownCleanupSurvivesRestart(t, i)
}

// TestReprovisionRemote_RevalidationFailureRetryReapsBeforeProvisioningAgain is
// the user-visible harm behind #3475, asserted directly: a retry after that
// failure must reap the sandbox it could not confirm gone BEFORE provisioning a
// replacement, so one restore attempt cannot leave two sandboxes running.
//
// Without the retained handle the retry sees nothing to reap and provisions a
// second sandbox alongside the first — a leaked container/remote workspace with
// no handle anywhere that can ever reach it.
func TestReprovisionRemote_RevalidationFailureRetryReapsBeforeProvisioningAgain(t *testing.T) {
	creds := &fakeCreds{invalid: errors.New("the listener moved while the sandbox was being provisioned")}
	var events []string
	teardowns := 0
	freshBackend := &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/fresh",
	}}
	rt := &orderedReprovisionRuntime{events: &events, res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error {
			teardowns++
			events = append(events, "teardown")
			if teardowns == 1 {
				return fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)
			}
			return nil
		},
	}}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		Title:   "s",
		Path:    t.TempDir(),
		Branch:  "root/s",
		backend: newInertSandboxBackend("sandbox"),
	}
	i.SetSandboxCredentials(creds)

	require.Error(t, i.reprovisionRemote())
	require.Equal(t, []string{"provision", "teardown"}, events)

	// The listener settles and the restore is retried.
	creds.invalid = nil
	require.NoError(t, i.reprovisionRemote())

	assert.Equal(t, []string{"provision", "teardown", "teardown", "provision"}, events,
		"the retry must retry the unconfirmed cleanup first; a second provision without it leaves two sandboxes running")

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.NotNil(t, i.remoteClient, "the settled retry must bind a live replacement")
	assert.False(t, i.runtimeCleanupStateUnknown, "a confirmed reap must clear the unknown marker")
}

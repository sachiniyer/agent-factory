package session

import (
	"encoding/json"
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

// TestReprovisionRemote_RevalidationFailureDegradesAndBindsReplacement covers
// the post-provision credential window under #5571 W3.1: the replacement came
// up, but the credential minted for it was invalidated while it was being
// provisioned. That used to reap the sandbox; now it DEGRADES — a callback-less
// session beats no session — so the dead grant is revoked for tidiness, the
// reason lands on the instance, and the live replacement is bound like any
// other.
func TestReprovisionRemote_RevalidationFailureDegradesAndBindsReplacement(t *testing.T) {
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

	require.NoError(t, i.reprovisionRemote())
	assert.Equal(t, 0, teardowns, "a healthy sandbox is never reaped for a callback-only failure")
	assert.Equal(t, 1, creds.revokes, "the dead grant is revoked, but the sandbox it was minted for stays")

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.Same(t, freshBackend, i.backend)
	assert.NotNil(t, i.remoteClient, "the replacement binds live; only its callback is absent")
	assert.NotNil(t, i.runtimeTeardown)
	assert.False(t, i.runtimeCleanupStateUnknown, "no teardown ran, so there is no outcome to doubt")
	assert.Equal(t, "the listener moved while the sandbox was being provisioned", i.callbackUnavailable,
		"the revalidation refusal must override the mint reason — it is the more specific cause")
}

// TestReprovisionRemote_RetryAfterRevalidationDegradationReapsFirst pins the
// ordering the degrade must not relax: a degraded (callback-less) replacement
// is still an owned sandbox, so the NEXT reprovision reaps it BEFORE
// provisioning again — one restore attempt can never leave two sandboxes
// running (#3475). A healthy second provision also clears the recorded reason.
func TestReprovisionRemote_RetryAfterRevalidationDegradationReapsFirst(t *testing.T) {
	creds := &fakeCreds{invalid: errors.New("the listener moved while the sandbox was being provisioned")}
	var events []string
	freshBackend := &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid",
		SessionDir: "/remote/af-sessions/fresh",
	}}
	rt := &orderedReprovisionRuntime{events: &events, res: ProvisionResult{
		Backend:  freshBackend,
		Endpoint: &AgentServerEndpoint{URL: "http://127.0.0.1:9", Token: "tok"},
		Teardown: func() error {
			events = append(events, "teardown")
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

	// The first restore comes up callback-less: revalidation failed, the dead
	// grant was revoked, and the sandbox was kept rather than reaped.
	require.NoError(t, i.reprovisionRemote())
	require.Equal(t, []string{"provision"}, events)
	assert.Equal(t, 1, creds.revokes)
	assert.NotEmpty(t, i.CallbackUnavailable())

	// The listener settles and the restore is retried: the degraded sandbox is
	// reaped before the replacement provisions.
	creds.invalid = nil
	require.NoError(t, i.reprovisionRemote())

	assert.Equal(t, []string{"provision", "teardown", "provision"}, events,
		"the retry must reap the degraded sandbox first; provisioning without it leaves two running")

	i.mu.RLock()
	defer i.mu.RUnlock()
	assert.NotNil(t, i.remoteClient, "the settled retry must bind a live replacement")
	assert.Empty(t, i.callbackUnavailable, "a healthy reprovision clears the recorded reason")
	assert.False(t, i.runtimeCleanupStateUnknown)
}

// TestReprovisionRemote_ConfirmedReapDropsDischargedCleanupHandle is the
// regression for the second review finding on ed3f86f that the maintainer ruled
// belongs in this PR (#5559 comment). Before this change ordinary sandbox rows
// never persisted RuntimeCleanup, so a retained-but-discharged cleanup identity
// could not reach disk. This PR persists the handle on ordinary sandbox rows so
// a restart can rebuild it — which means a confirmed reap that leaves the old
// backend in place (a re-provision that fails before binding its replacement,
// here the mint) would otherwise carry the discharged handle to the next
// checkpoint, and a restart would rebuild a teardown for a runtime af already
// reaped and act on a resource it no longer owns.
//
// Property the fix must satisfy (from the review): once a reap is confirmed, no
// later checkpoint persists that runtime's cleanup identity, and the in-flight-
// archive rule at storage_checkpoint.go (no manufactured unknown-cleanup boundary
// after teardown) still holds. resetRemoteRuntime clears the backend's immutable
// cleanup identity under i.mu when the reap is confirmed, so the staged handle
// is nil and no unknown flag is set; the reloaded row carries nothing to rebuild.
func TestReprovisionRemote_ConfirmedReapDropsDischargedCleanupHandle(t *testing.T) {
	// A confirmed reap: the old teardown returns nil (a known, completed outcome).
	// The sandbox backend carries a cleanup identity the confirmed reap must
	// discharge; a docker backend does not inject a sandbox callback, so its mint
	// never fails and it cannot reach the rebind-failure window this test drives.
	oldBackend := &sandboxBackend{
		cleanup: &SandboxRuntimeCleanupData{
			SSHCommand: "ssh -o User='ci' sandbox.example.test",
			SessionDir: "/srv/af/reaped.1234",
			RemotePID:  cleanupFixtureAltPID,
		},
	}
	teardowns := 0
	i := &Instance{
		Title:   "s",
		Path:    t.TempDir(),
		Branch:  "root/s",
		backend: oldBackend,
		runtimeTeardown: func() error {
			teardowns++
			return nil // confirmed reap
		},
	}
	// A credential whose Mint fails is the rebind failure: the old runtime is
	// reaped (confirmed), then reprovisionRemote returns before binding a
	// replacement, leaving the old (now-discharged) backend in place.
	i.SetSandboxCredentials(&fakeCreds{mintErr: errors.New("the minter is offline")})
	rt := &orderedReprovisionRuntime{
		res: ProvisionResult{Backend: &sandboxBackend{cleanup: &SandboxRuntimeCleanupData{
			SSHCommand: "ssh -o User='ci' sandbox.example.test",
			SessionDir: "/srv/af/fresh.1234",
		}}},
	}
	restoreRuntime := SetRuntimeForTest(BackendSandbox, func() Runtime { return rt })
	defer restoreRuntime()

	err := i.reprovisionRemote()
	require.ErrorContains(t, err, "callback credential could not be re-issued")
	assert.Equal(t, 1, teardowns, "the confirmed reap must run before the rebind fails")

	i.mu.RLock()
	backend := i.backend
	teardown := i.runtimeTeardown
	i.mu.RUnlock()
	// The rebind failure leaves the old backend in place, but the confirmed reap
	// discharged its runtime: the teardown handle is gone and the backend's
	// immutable cleanup identity is cleared, so the next checkpoint stages no
	// handle for a runtime af already reaped.
	assert.Same(t, oldBackend, backend, "the rebind failure left the old backend in place; a replacement was never bound")
	assert.Nil(t, teardown, "the confirmed reap cleared the in-memory teardown handle")
	if provider, ok := backend.(runtimeCleanupProvider); ok {
		assert.Nil(t, provider.runtimeCleanupData(), "the confirmed reap cleared the backend's discharged cleanup identity; a later checkpoint must not persist it")
	}

	// The next checkpoint publishes no RuntimeCleanup for the reaped runtime and
	// does not manufacture an unknown-cleanup boundary (storage_checkpoint.go
	// gates that on a live cleanup handle, which the confirmed reap discharged).
	stored := i.ToInstanceData().ForStorage()
	assert.Nil(t, stored.RuntimeCleanup, "the checkpoint persisted the reaped runtime's cleanup identity; a restart would rebuild a teardown for a runtime af already reaped")
	assert.False(t, stored.RuntimeCleanupStateUnknown, "the checkpoint manufactured an unknown-cleanup boundary for a confirmed reap; the in-flight-archive rule must not fire without a live handle")
	assert.False(t, stored.KilledWithoutLiveRuntime, "an ordinary row staged the nothing-owed marker; the marker is a tombstone property")

	// The restart carries no rebuildable handle for the reaped runtime: the
	// reloaded row loads inert, with no RuntimeCleanup and no rebuilt teardown.
	raw, err := json.Marshal(stored)
	require.NoError(t, err, "marshal discharged-cleanup checkpoint")
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded), "unmarshal discharged-cleanup checkpoint")
	reloaded = reloaded.ForStorage()
	assert.Nil(t, reloaded.RuntimeCleanup, "the reloaded row still carries a cleanup handle for the reaped runtime")
	restored, err := FromInstanceData(reloaded)
	require.NoError(t, err, "restore the reloaded row")
	assert.Nil(t, restored.runtimeTeardown, "the reloaded row rebuilt a teardown handle for a runtime af already reaped")
}

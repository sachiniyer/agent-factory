package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The provision_cmd path pins one host key per session under hook-hosts/<slug>,
// and the LIVE teardown drops that directory when delete_cmd succeeds. A
// tombstone restored after a daemon restart rebuilds the reap from storage, so
// it has to reach the same end state — otherwise every kill that outlives its
// daemon leaves a directory under the af home forever (#3454).
//
// These tests round-trip the record exactly as the daemon does
// (ToInstanceData().ForStorage() -> JSON -> JSON -> FromInstanceData) and drive
// the real constructors on both ends: the pin is written by the production
// hookProvisionKnownHosts under the production hookProvisionSessionDir, and the
// tombstone is staged by provisionedBackend rather than a hand-built literal, so
// an assertion here cannot be satisfied by restating the code under test.

// hookPinnedProvisionSession stages one provision_cmd session: the real pinned
// host-key directory, and the real Backend whose cleanup handle is what storage
// will carry. deleteBody is the body of delete_cmd, which is what decides
// whether the pin may be dropped.
func hookPinnedProvisionSession(t *testing.T, deleteBody string) (inst *Instance, dir string, log string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	record := `{"host":"10.0.0.7","host_key":"` + provisionKey + `"}`
	h := newHookState(t, "printf '%s' '"+record+"'\n", deleteBody)
	p := newHookProvisioner(h, t.Name())
	// A provision_cmd session, not a launch_cmd one: only this contract pins a
	// host key, and only it may claim the directory.
	p.hooks.LaunchCmd = ""
	p.hooks.ProvisionCmd = h.launch
	// The restored reap runs delete_cmd unconditionally; the live one is gated on
	// launch having started.
	p.launchStarted = true

	dir, err := hookProvisionSessionDir(p.slug)
	require.NoError(t, err)
	// The production create path, so the directory under test is the one af
	// actually makes rather than a path this test spelled itself.
	_, err = hookProvisionKnownHosts(dir, "10.0.0.7", 0, provisionKey)
	require.NoError(t, err)
	require.DirExists(t, dir, "the pin must exist before the teardown that owns it runs")

	// The LIVE teardown must never run here: everything below exercises the
	// closure rebuilt from storage, and a test that silently fell back to the
	// in-memory one would prove nothing about a restart.
	live := func() error {
		t.Error("the restored tombstone used the live teardown closure, which no daemon restart can preserve")
		return nil
	}
	return &Instance{
		ID:              t.Name() + "-id",
		Title:           t.Name(),
		Path:            "/repo",
		backend:         p.provisionedBackend(live),
		runtimeTeardown: live,
		userKilled:      true,
	}, dir, filepath.Join(h.dir, "delete-ran.log")
}

// restoreHookTombstone puts the record through the daemon's real retention path:
// staged on the instance, published by ForStorage, written and read back as
// JSON, normalized again the way Storage.LoadInstances does, and rebuilt.
func restoreHookTombstone(t *testing.T, inst *Instance) *Instance {
	t.Helper()
	raw, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)

	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	stored = stored.ForStorage()
	require.NotNil(t, stored.RuntimeCleanup, "the tombstone lost its durable cleanup handle")

	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown, "the restored tombstone has no teardown to finish")
	return restored
}

// The headline #3454 regression: delete_cmd succeeds after a restart, so the
// machine is gone and the pin has nothing left to verify. It must go with it.
func TestRestoredProvisionTombstoneRemovesPinnedHostKeyDirectory(t *testing.T) {
	inst, dir, deleteLog := hookPinnedProvisionSession(t, "exit 0")

	restored := restoreHookTombstone(t, inst)
	require.NoError(t, restored.Kill(), "a delete_cmd that succeeds must complete the restored teardown")

	assert.FileExists(t, deleteLog, "delete_cmd never ran, so this proves nothing about its success path")
	assert.NoDirExists(t, dir,
		"a tombstone restored after a daemon restart leaked its pinned host-key directory (#3454)")
}

// The constraint the fix must not weaken. The pin is embedded in this session's
// sandbox ssh command, so dropping it while cleanup is still retryable would
// make every later reap fail host-key verification before it could reach the
// machine — a retained row that could never complete. Without this test a later
// "simplification" could hoist the removal out of the success branch and nothing
// would notice.
func TestRestoredProvisionTombstoneKeepsPinnedDirectoryWhenDeleteCmdFails(t *testing.T) {
	inst, dir, deleteLog := hookPinnedProvisionSession(t, "exit 7")

	restored := restoreHookTombstone(t, inst)
	err := restored.Kill()
	require.Error(t, err, "a delete_cmd that failed must not report a completed teardown")
	assert.ErrorContains(t, err, "delete_cmd failed",
		"the retain-and-retry error the tombstone is classified by must reach the caller unchanged")

	assert.FileExists(t, deleteLog, "delete_cmd never ran, so this proves nothing about its failure path")
	assert.DirExists(t, dir, "the pin must survive a retryable teardown failure, or no retry can ever reach the machine")
	assert.FileExists(t, filepath.Join(dir, "known_hosts"),
		"the pinned key itself must survive — the directory alone verifies nothing")
}

// The trap in this issue's own recommended fix. cleanupData() is shared by BOTH
// hook contracts, so a flag set there unconditionally would make a launch_cmd
// tombstone claim a directory it never created: launch_cmd owns a URL and a
// token, never a pinned host key. Under a slug it does not own, that claim
// deletes someone else's pin.
func TestRestoredLaunchTombstoneDoesNotClaimAPinnedHostKeyDirectory(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	h := newHookState(t, `printf '%s' '{"url":"http://127.0.0.1:9","token":"unused"}'`+"\n", "exit 0")
	p := newHookProvisioner(h, t.Name())
	// The launch_cmd contract, driven through its real constructor.
	res, err := p.provision()
	require.NoError(t, err)

	// A pinned directory under this session's slug that this session did NOT
	// create — what a recycled slug or a stale pin looks like on disk.
	dir, err := hookProvisionSessionDir(p.slug)
	require.NoError(t, err)
	_, err = hookProvisionKnownHosts(dir, "10.0.0.7", 0, provisionKey)
	require.NoError(t, err)

	restored := restoreHookTombstone(t, &Instance{
		ID:              t.Name() + "-id",
		Title:           t.Name(),
		Path:            "/repo",
		backend:         res.Backend,
		runtimeTeardown: res.Teardown,
		userKilled:      true,
	})
	require.NoError(t, restored.Kill())

	assert.DirExists(t, dir,
		"a launch_cmd tombstone removed a pinned host-key directory it never created")
	if _, err := os.Stat(filepath.Join(dir, "known_hosts")); err != nil {
		t.Fatalf("the pin a launch_cmd session does not own was destroyed: %v", err)
	}
}

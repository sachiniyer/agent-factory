package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// One --account create resolves its target host and its licence exactly once
// (#7304). The admission gate, the factory, the account sync, the provision and
// the durable record all act on that one answer: a session recorded under
// account X on host H is provisioned on H under X, or the create fails before
// any credential is synced or any workspace is made.
//
// These tests move default_host from one LISTED host to another between the
// licence read and Provision. Both hosts are licensed, so no "is it listed?"
// check can see the change; only the host's identity can.

// twoListedHosts gives each host an identity file that does not exist, under
// dir. The REAL ssh runtime refuses a missing identity file after it has chosen
// its host and before it contacts anything, and the refusal names the path — so
// the error reports which host the runtime was about to provision on, without a
// network or a fake runtime standing in for the code under test.
func twoListedHosts(dir string) map[string]config.HostConfig {
	return map[string]config.HostConfig{
		"box1": {Host: "box1.example", User: "dev", IdentityFile: filepath.Join(dir, "box1-key")},
		"box2": {Host: "box2.example", User: "dev", IdentityFile: filepath.Join(dir, "box2-key")},
	}
}

// saveDefaultHost rewrites the global config with the given host list and
// default_host.
func saveDefaultHost(t *testing.T, hosts map[string]config.HostConfig, defaultHost string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Hosts = hosts
	cfg.DefaultHost = defaultHost
	require.NoError(t, config.SaveConfig(cfg))
}

// flipDefaultHostBeforeProvision wraps the backend factory so default_host moves
// to `to` AFTER NewInstance's licence read and BEFORE the factory (and so the
// runtime's Provision) runs.
func flipDefaultHostBeforeProvision(t *testing.T, hosts map[string]config.HostConfig, to string) {
	t.Helper()
	t.Cleanup(SetProvisionResultFactoryForTest(func(opts InstanceOptions, absPath string, kind BackendKind) (ProvisionResult, error) {
		saveDefaultHost(t, hosts, to)
		return defaultBackendFactoryForKind(opts, absPath, kind)
	}))
}

// The real ssh runtime provisions an --account create on the host the licence
// was read for, not on whichever listed host default_host names by the time
// Provision runs.
func TestAccountCreateProvisionsOnTheLicensedHostAfterDefaultHostMoves(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box1")
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	// The fixture installs a capturing runtime; this test is about the real one.
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return sshRuntime{} }))
	flipDefaultHostBeforeProvision(t, hosts, "box2")

	_, err := f.create(t, InstanceOptions{Account: "work"}) // no --host: default_host applies
	require.Error(t, err, "both identity files are missing, so the real runtime stops before contacting a host")
	assert.NotContains(t, err.Error(), "box2-key",
		"the runtime chose box2: the account was licensed for box1, so its credentials and workspace were headed to a host the gate never read")
	assert.Contains(t, err.Error(), "box1-key",
		"the runtime acts on the licensed host, box1")
}

// The host the durable row records is the host handed to Provision, so archive,
// restore and re-provision address the machine the workspace and the credentials
// were actually put on.
func TestRecordedHostIsTheHostProvisionWasGiven(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box1")
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	flipDefaultHostBeforeProvision(t, hosts, "box2")

	*f.spec = ProvisionSpec{}
	inst, err := f.create(t, InstanceOptions{Account: "work"})
	require.NoError(t, err)
	assert.Equal(t, "box1", inst.Host, "the row pins the licensed host")
	assert.Equal(t, inst.Host, f.spec.Host,
		"Provision is handed the host the row records, not an empty pick it would re-resolve from a config that has since changed")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "under the account the row records")
}

// A create WITHOUT --account is unchanged: an empty pick stays empty all the way
// to the runtime, which resolves it itself, and the row records the pick as
// made. It makes no licence read, so there is no earlier answer to hold it to.
func TestAccountlessCreateStillLetsTheRuntimeResolveTheHost(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box1")
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})

	*f.spec = ProvisionSpec{}
	inst, err := f.create(t, InstanceOptions{})
	require.NoError(t, err)
	assert.Empty(t, f.spec.Host, "the runtime receives the empty pick and resolves default_host itself")
	assert.Empty(t, inst.Host, "the row records the pick as made")
	assert.Empty(t, f.spec.Account.Dir)

	// And the real runtime follows the config it reads at Provision.
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return sshRuntime{} }))
	flipDefaultHostBeforeProvision(t, hosts, "box2")
	_, err = f.create(t, InstanceOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "box2-key", "an accountless create goes where default_host points when the runtime reads it")
}

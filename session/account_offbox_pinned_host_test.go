package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
	assert.Equal(t, inst.Host, f.spec.AccountHost,
		"and is told to hold itself to that host")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "under the account the row records")
}

// An explicit --host pick is threaded the same way: the pick, the licensed host
// and the recorded host are one name.
func TestExplicitHostPickIsTheLicensedHostProvisionIsHeldTo(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box2")

	*f.spec = ProvisionSpec{}
	inst, err := f.create(t, InstanceOptions{Account: "work", Host: "box1"})
	require.NoError(t, err)
	assert.Equal(t, "box1", inst.Host)
	assert.Equal(t, "box1", f.spec.Host)
	assert.Equal(t, "box1", f.spec.AccountHost)
}

// The licensed host can be gone by the time Provision runs. The create fails,
// naming the host the account was licensed for, rather than falling through to
// another target.
func TestAccountCreateFailsWhenTheLicensedHostIsRemovedBeforeProvision(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box1")
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return sshRuntime{} }))
	onlyBox2 := map[string]config.HostConfig{"box2": hosts["box2"]}
	flipDefaultHostBeforeProvision(t, onlyBox2, "box2")

	_, err := f.create(t, InstanceOptions{Account: "work"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `licensed for host "box1"`)
	assert.Contains(t, err.Error(), "retry the create")
	assert.NotContains(t, err.Error(), "box2-key", "the runtime never got as far as preparing to reach box2")
}

// The runtime holds itself to the licensed host even when its caller did not
// thread the pick: whatever it resolves must be the host the account was
// licensed for. Each refusal lands before the identity-file check, which is the
// first thing Provision does with a chosen host — so nothing has been contacted,
// synced or created.
func TestSSHProvisionRefusesAnAccountOnAnyHostButTheLicensedOne(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box2")
	spec := func(host, licensed string) ProvisionSpec {
		return ProvisionSpec{
			RepoRoot: f.repo, Title: "t", Program: tmux.ProgramClaude, Host: host, AccountHost: licensed,
			CloneURL: "https://example.invalid/fixture.git",
			Account:  sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: f.accountDir},
		}
	}
	refused := func(t *testing.T, s ProvisionSpec) string {
		t.Helper()
		_, err := sshRuntime{}.Provision(s)
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, `"work"`, "names the account")
		assert.Contains(t, msg, "retry the create", "says what to do")
		assert.Contains(t, msg, "No credentials were sent and no workspace was created")
		assert.NotContains(t, msg, "-key", "refused before the chosen host's settings were used")
		return msg
	}

	t.Run("another listed host", func(t *testing.T) {
		// An empty pick re-resolved through default_host, now box2.
		msg := refused(t, spec("", "box1"))
		assert.Contains(t, msg, `licensed for host "box1"`)
		assert.Contains(t, msg, `resolved host "box2"`, "names both hosts")
	})
	t.Run("an explicit pick of another listed host", func(t *testing.T) {
		msg := refused(t, spec("box2", "box1"))
		assert.Contains(t, msg, `licensed for host "box1"`)
		assert.Contains(t, msg, `resolved host "box2"`)
	})
	t.Run("the repository's own ssh target", func(t *testing.T) {
		writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "repo-chosen.example"}})
		t.Cleanup(func() { writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"}) })
		msg := refused(t, spec("", "box1"))
		assert.Contains(t, msg, `licensed for host "box1"`)
		assert.Contains(t, msg, "the repository's own [ssh] target")
	})
	t.Run("no licensed host at all", func(t *testing.T) {
		msg := refused(t, spec("box1", ""))
		assert.Contains(t, msg, "without the host it was licensed for")
	})

	// Control: the licensed host itself passes the check and reaches the
	// identity-file refusal, which is past it.
	_, err := sshRuntime{}.Provision(spec("box1", "box1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "box1-key")
	assert.NotContains(t, err.Error(), "licensed for")

	// And a spec with no account is never held to anything.
	_, err = sshRuntime{}.Provision(ProvisionSpec{
		RepoRoot: f.repo, Title: "t", Program: tmux.ProgramClaude,
		CloneURL: "https://example.invalid/fixture.git",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "box2-key", "an accountless provision follows default_host")
}

// The factory hands a LOCAL create's host pick through untouched and names no
// licensed host: the in-target shim resolves the account, and nothing here
// places it.
func TestFactoryLeavesALocalCreatesHostPickAlone(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	spec := &ProvisionSpec{}
	t.Cleanup(SetRuntimeForTest(BackendLocal, func() Runtime { return capturingRuntime{got: spec} }))
	_, err := defaultBackendFactoryForKind(InstanceOptions{
		Title: "local-acct", Path: f.repo, Program: tmux.ProgramClaude, Account: "work",
	}, f.repo, BackendLocal)
	require.NoError(t, err)
	assert.Empty(t, spec.Host)
	assert.Empty(t, spec.AccountHost)
	assert.Empty(t, spec.Account.Dir)
}

// An ssh account that reaches the factory carried but with no licensed host
// cannot have come from the gate; the factory refuses rather than let the
// runtime pick a host for it.
func TestFactoryRefusesACarriedSSHAccountWithNoLicensedHost(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	*f.spec = ProvisionSpec{}
	_, err := defaultBackendFactoryForKind(InstanceOptions{
		Title: "no-host", Path: f.repo, Program: tmux.ProgramClaude, Account: "work", AccountCarried: true,
	}, f.repo, BackendSSH)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "named no host")
	assert.Empty(t, f.spec.Account.Dir, "the runtime was never called")
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

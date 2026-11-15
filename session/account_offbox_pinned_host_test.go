package session

import (
	"path/filepath"
	"strings"
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
	assert.Contains(t, err.Error(), "Retry the create", "the create path names its own remedy")
	assert.Contains(t, err.Error(), "appears to have changed", "what af infers is phrased as an inference")
	assert.Equal(t, 1, strings.Count(err.Error(), "backend=ssh:"), "the embedded host-list error does not repeat the prefix")
	assert.NotContains(t, err.Error(), "sandbox was already retired", "nothing was retired by a create")
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
		assert.NotContains(t, msg, "Retry the create",
			"the runtime does not know which operation called it, so it names no remedy; the caller adds one")
		assert.NotContains(t, msg, "already retired")
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
		assert.Contains(t, msg, "bug in af", "a caller defect is reported as one")
		_, err := sshRuntime{}.Provision(spec("box1", ""))
		assert.Equal(t, err, withAccountHostRemedy(err, createAccountHostRemedy),
			"and gets no remedy from either caller: a retry would repeat it")
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
	assert.Contains(t, err.Error(), "bug in af")
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

// accountRow is a durable ssh row under account "work", as a restore finds it.
func accountRow(f listedFixture, host string, reap func() error) *Instance {
	if reap == nil {
		reap = func() error { return nil }
	}
	return &Instance{
		Title: "s", Path: f.repo, Branch: "root/s", Host: host, Program: tmux.ProgramClaude,
		Account: "work", backend: newInertSandboxBackend("ssh"), runtimeTeardown: reap,
	}
}

// A re-provision of an account row hands the runtime Host == AccountHost == the
// host its one licence read resolved (#7327). Without the pin the runtime
// refuses every account-bearing ssh restore, after the old sandbox is reaped.
func TestReprovisionPinsTheHostItsOneReadResolved(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	saveDefaultHost(t, hosts, "box1")
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"})

	for name, tc := range map[string]struct{ rowHost, want string }{
		"a row pinned to its host":                    {"box2", "box2"},
		"a row with an empty host under default_host": {"", "box1"},
	} {
		t.Run(name, func(t *testing.T) {
			*f.spec = ProvisionSpec{}
			require.NoError(t, accountRow(f, tc.rowHost, nil).reprovisionRemote())
			assert.Equal(t, tc.want, f.spec.Host, "the runtime is handed the host as an explicit pick")
			assert.Equal(t, tc.want, f.spec.AccountHost, "and is held to it")
			assert.Equal(t, f.accountDir, f.spec.Account.Dir)
		})
	}

	// And the REAL runtime accepts what re-provision hands it: it gets past the
	// licensed-host check to the identity-file refusal for that host.
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return sshRuntime{} }))
	err := accountRow(f, "", nil).reprovisionRemote()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "box1-key")
	assert.NotContains(t, err.Error(), "licensed for")
}

// A restore's licence read runs before the reap, so a host that is already gone
// is refused with the old sandbox intact. A host removed AFTER that read is
// caught by the runtime, which runs after the reap — and the refusal then has to
// be true for a restore: it names the re-provision, says the previous sandbox
// was already retired, and does not tell anyone to retry a create.
func TestRestoreRefusalAfterTheReapSaysTheSandboxWasRetired(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return sshRuntime{} }))
	reaped := false
	// The reap sits between the licence read and Provision, so it is where the
	// host list changes.
	row := accountRow(f, "box1", func() error {
		reaped = true
		saveDefaultHost(t, map[string]config.HostConfig{"box2": hosts["box2"]}, "box2")
		return nil
	})

	err := row.reprovisionRemote()
	require.Error(t, err)
	msg := err.Error()
	require.True(t, reaped, "premise: the refusal came from the runtime, after the reap")
	assert.Contains(t, msg, "failed to re-provision sandbox", "names the operation it refused")
	assert.Contains(t, msg, `licensed for host "box1"`)
	assert.Contains(t, msg, "previous sandbox was already retired")
	assert.Contains(t, msg, "No credentials were sent and no workspace was created")
	assert.NotContains(t, strings.ToLower(msg), "the create", "there is no create to retry")
	assert.NotContains(t, msg, "being created")
	assert.Equal(t, 1, strings.Count(msg, "backend=ssh:"))
	assert.NotContains(t, msg, "box2-key", "and nothing was attempted on another host")
}

// An adopting provision reaches its host through the transport the archived row
// recorded, so the licensed-host refusal does not apply to it — a host list that
// changed must not stop a reattach. What still applies is the rule: it sends the
// account under the licensed name, or it sends nothing (#7327).
func TestAdoptionSendsTheAccountUnderTheLicensedNameOrNotAtAll(t *testing.T) {
	hosts := twoListedHosts(t.TempDir())
	f := newListedFixture(t, hosts)
	adopting := func(host, licensed string) ProvisionSpec {
		return ProvisionSpec{
			RepoRoot: f.repo, Title: "t", Program: tmux.ProgramClaude, Host: host, AccountHost: licensed,
			Account: sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: f.accountDir},
			Adopt:   &AdoptSpec{SessionDir: "/home/dev/.af-sessions/x", Branch: "root/s"},
		}
	}
	sends := func(spec ProvisionSpec) (bool, error) {
		return (&sandboxWorkspace{spec: spec}).accountSyncApplies()
	}

	t.Run("the licensed host is still listed", func(t *testing.T) {
		spec := adopting("box1", "box1")
		name, listed, err := sshProvisionHost(&spec, mustResolve(t, f.repo))
		require.NoError(t, err)
		require.NotNil(t, listed)
		assert.Equal(t, "box1", name)
		assert.Equal(t, "box1", spec.AccountSyncHost, "the sync is licensed under the licensed name")
		ok, err := sends(spec)
		require.NoError(t, err)
		assert.True(t, ok, "so the account is sent")
	})

	t.Run("another listed host resolves instead", func(t *testing.T) {
		spec := adopting("box2", "box1")
		_, _, err := sshProvisionHost(&spec, mustResolve(t, f.repo))
		require.NoError(t, err, "an adoption is not refused: its transport is the recorded one")
		assert.Empty(t, spec.AccountSyncHost, "but a host other than the licensed one earns no licence")
		assert.Empty(t, spec.AgentConfigFiles)
		ok, err := sends(spec)
		assert.False(t, ok)
		require.Error(t, err, "so nothing is sent")
	})

	t.Run("the licensed host was removed from the list", func(t *testing.T) {
		saveDefaultHost(t, map[string]config.HostConfig{"box2": hosts["box2"]}, "")
		spec := adopting("box1", "box1")
		name, listed, err := sshProvisionHost(&spec, mustResolve(t, f.repo))
		require.NoError(t, err, "the changed host list does not stop the adoption")
		assert.Nil(t, listed)
		assert.Empty(t, name)
		assert.Empty(t, spec.AccountSyncHost)
		ok, err := sends(spec)
		assert.False(t, ok)
		require.Error(t, err, "and nothing is sent")

		// The real runtime agrees: it gets past the host check and stops at the
		// first thing an adoption needs, the recorded transport.
		_, err = sshRuntime{}.Provision(adopting("box1", "box1"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "recorded no transport")
		assert.NotContains(t, err.Error(), "licensed for")

		// Control: the same spec without Adopt is a fresh provision, and is refused.
		fresh := adopting("box1", "box1")
		fresh.Adopt = nil
		_, _, err = sshProvisionHost(&fresh, mustResolve(t, f.repo))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `licensed for host "box1"`)
	})

	// An accountless adoption keeps its licence for agent config on any listed host.
	saveDefaultHost(t, hosts, "")
	spec := adopting("box2", "")
	spec.Account = sessionenv.Account{}
	_, _, err := sshProvisionHost(&spec, mustResolve(t, f.repo))
	require.NoError(t, err)
	assert.Equal(t, "box2", spec.AccountSyncHost)
}

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// capturingRuntime is an ssh runtime that records the spec it was handed.
type capturingRuntime struct{ got *ProvisionSpec }

func (c capturingRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	*c.got = spec
	return ProvisionResult{Backend: &LocalBackend{}}, nil
}

type listedFixture struct {
	repo       string
	accountDir string
	spec       *ProvisionSpec
}

// newListedFixture: an af home with a registered claude account "work" and the
// global host list [box1], a repo with an origin, and a capturing ssh runtime.
func newListedFixture(t *testing.T, hosts map[string]config.HostConfig) listedFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Hosts = hosts
	require.NoError(t, config.SaveConfig(cfg))
	dir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/ssh", nil }))
	spec := &ProvisionSpec{}
	t.Cleanup(SetRuntimeForTest(BackendSSH, func() Runtime { return capturingRuntime{got: spec} }))
	return listedFixture{repo: repo, accountDir: dir, spec: spec}
}

func (f listedFixture) create(t *testing.T, opts InstanceOptions) (*Instance, error) {
	t.Helper()
	opts.Title, opts.Path, opts.Program = "acct-offbox", f.repo, tmux.ProgramClaude
	return NewInstance(opts)
}

var oneListedHost = map[string]config.HostConfig{"box1": {Host: "box1.example", User: "dev"}}

// Only a LISTED host licenses an account — and the licence is the runtime's own
// host resolution, so a repository can never supply it.
func TestListedAccountHostIsTheRuntimesOwnResolution(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	name, ok := listedAccountHost(BackendSSH, f.repo, "box1")
	assert.True(t, ok)
	assert.Equal(t, "box1", name)

	_, ok = listedAccountHost(BackendSSH, f.repo, "nope")
	assert.False(t, ok, "a host that is not in the list is not licensed")
	_, ok = listedAccountHost(BackendSandbox, f.repo, "box1")
	assert.False(t, ok, "a free-form sandbox command cannot be listed")
	_, ok = listedAccountHost(BackendHook, f.repo, "box1")
	assert.False(t, ok)
	_, ok = listedAccountHost(BackendLocal, f.repo, "box1")
	assert.False(t, ok)

	// A target the REPOSITORY chose — its own [ssh] section — is not licensed, even
	// with a perfectly good host list in the operator's config.
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "repo-chosen.example"}})
	_, ok = listedAccountHost(BackendSSH, f.repo, "")
	assert.False(t, ok, "nothing repo-chosen can pick the destination of credentials")
	// ...while an explicit pick of a listed host still is.
	_, ok = listedAccountHost(BackendSSH, f.repo, "box1")
	assert.True(t, ok)
}

// --account on a listed host is admitted, resolved to the registry directory on
// THIS machine, and handed to the runtime with the host that licenses the sync.
func TestAccountOnAListedHostIsAdmittedAndResolved(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	inst, err := f.create(t, InstanceOptions{Account: "work", Host: "box1"})
	require.NoError(t, err)
	assert.Equal(t, "work", inst.Account, "the session records the account it runs under")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "the registry directory, resolved on the daemon box")
	assert.Equal(t, "work", f.spec.Account.Name)
}

// An explicit --account on a target that is not a listed host is refused at
// admission with the actionable remedy, and nothing is provisioned.
func TestAccountOnAnUnlistedTargetIsRefusedAtAdmission(t *testing.T) {
	f := newListedFixture(t, oneListedHost)

	// An unlisted ssh target: the repo's own [ssh] section.
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "repo-chosen.example"}})
	_, err := f.create(t, InstanceOptions{Account: "work"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global host list")
	assert.Contains(t, err.Error(), "--host")
	assert.Empty(t, f.spec.Account.Dir, "nothing was provisioned, so nothing was sent")

	// A sandbox / hook target is refused too.
	for _, kind := range []BackendKind{BackendSandbox, BackendHook} {
		_, err = f.create(t, InstanceOptions{Account: "work", Backend: kind})
		require.Error(t, err, kind)
	}
}

// A DEFAULT account never makes a create impossible: on a target that cannot
// carry it the session runs without it (and the operator is told in the log),
// while the same default on a listed host is carried like an explicit one.
func TestDefaultAccountNeverBlocksAnOffBoxCreate(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	const source = "this session's account was not requested — it comes from default_accounts.claude"

	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "repo-chosen.example"}})
	inst, err := f.create(t, InstanceOptions{Account: "work", AccountSource: source})
	require.NoError(t, err, "a default account must not fail an unlisted off-box create")
	assert.Empty(t, inst.Account, "the unlisted target does not receive it")
	assert.Empty(t, f.spec.Account.Dir, "and no credential is sent")

	*f.spec = ProvisionSpec{}
	inst, err = f.create(t, InstanceOptions{Account: "work", AccountSource: source, Host: "box1"})
	require.NoError(t, err)
	assert.Equal(t, "work", inst.Account, "the same default on a listed host is carried")
	assert.Equal(t, f.accountDir, f.spec.Account.Dir)
}

// A restore or Lost recovery re-provisions through the same licence, so the
// persisted account is carried again — never silently dropped, never sent to a
// target that is not listed.
func TestReprovisionCarriesThePersistedAccountOnlyOnAListedHost(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	build := func(host string) *Instance {
		return &Instance{
			Title: "s", Path: f.repo, Branch: "root/s", Host: host, Program: tmux.ProgramClaude,
			Account: "work", backend: newInertSandboxBackend("ssh"),
			runtimeTeardown: func() error { return nil },
		}
	}

	require.NoError(t, build("box1").reprovisionRemote())
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "the persisted account is resolved again on a listed host")
	assert.Equal(t, "box1", f.spec.Host)

	*f.spec = ProvisionSpec{}
	require.NoError(t, build("").reprovisionRemote())
	assert.Empty(t, f.spec.Account.Dir, "a target that is not listed receives no credential on a restore either")
}

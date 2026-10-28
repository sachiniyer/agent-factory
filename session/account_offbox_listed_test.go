package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
	err := build("").reprovisionRemote()
	require.Error(t, err, "a restore on a target that is not listed is refused, never launched under another identity")
	assert.Empty(t, f.spec.Account.Dir, "and no credential is sent")
}

// A session recorded under account X is relaunched under X or NOT AT ALL. When the
// target can no longer carry it, restore refuses BEFORE reaping anything — it never
// launches under the host's own login, another account, or none.
func TestRestoreRefusesWhenThePersistedAccountCannotBeCarried(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	reaped := false
	i := &Instance{
		Title: "s", Path: f.repo, Branch: "root/s", Host: "box1", Program: tmux.ProgramClaude,
		Account: "work", backend: newInertSandboxBackend("ssh"),
		runtimeTeardown: func() error { reaped = true; return nil },
	}

	// The host is no longer listed (the operator removed it from the global config).
	cfg := config.DefaultConfig()
	require.NoError(t, config.SaveConfig(cfg))
	err := i.reprovisionRemote()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `account "work"`)
	assert.Contains(t, err.Error(), "global host list")
	assert.Contains(t, err.Error(), "box1", "the recorded host is named so the operator can re-add it")
	assert.False(t, reaped, "the old runtime must still be there: a refusal costs nothing")
	assert.Empty(t, f.spec.RepoRoot, "and no replacement was provisioned")
}

func TestRestoreRefusesWhenThePersistedAccountIsGone(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	reaped := false
	i := &Instance{
		Title: "s", Path: f.repo, Branch: "root/s", Host: "box1", Program: tmux.ProgramClaude,
		Account: "work", backend: newInertSandboxBackend("ssh"),
		runtimeTeardown: func() error { reaped = true; return nil },
	}
	require.NoError(t, os.RemoveAll(f.accountDir)) // the account was removed from the registry

	err := i.reprovisionRemote()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persisted account")
	assert.False(t, reaped)
	assert.Empty(t, f.spec.RepoRoot)
}

// A session placed by default_host records the host it RESOLVED to, so a later
// change elsewhere — the repository gaining its own [ssh] section — cannot move the
// session, and its credentials, to a different target on restore.
func TestDefaultHostIsPinnedForAnAccountScopedSession(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.DefaultHost = "box1"
	require.NoError(t, config.SaveConfig(cfg))

	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh"}) // ssh, no [ssh] section: default_host applies
	inst, err := f.create(t, InstanceOptions{Account: "work"})     // no --host
	require.NoError(t, err)
	assert.Equal(t, "box1", inst.Host, "the resolved host is recorded, not the empty pick")

	// The repository now names its own ssh target; a re-derivation would follow it.
	writeInRepoConfig(t, f.repo, map[string]any{"backend": "ssh", "ssh": map[string]any{"host": "repo-chosen.example"}})
	inst.SetBackend(newInertSandboxBackend("ssh"))
	inst.runtimeTeardown = func() error { return nil }
	*f.spec = ProvisionSpec{}
	require.NoError(t, inst.reprovisionRemote())
	assert.Equal(t, f.accountDir, f.spec.Account.Dir, "the account still goes to the recorded listed host")
	assert.Equal(t, "box1", f.spec.Host)
}

// af must never hand an account's credentials to a program it did not choose. A
// program_overrides command that sets an identity variable itself (or that af
// cannot prove is a direct invocation of the agent) is refused BEFORE anything is
// synced — and before the host is contacted.
func TestUnprovenAgentCommandIsRefusedBeforeAnySync(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	for name, override := range map[string]string{
		"sets the config dir itself": "CLAUDE_CONFIG_DIR=/other claude",
		"a wrapper af cannot prove":  "./run-claude.sh",
		"a daemon-qualified path":    "/home/daemon/bin/claude",
		"undeclared extra arguments": "claude --some-flag",
		"a different agent entirely": "codex",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Hosts = oneListedHost
			cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: override}
			require.NoError(t, config.SaveConfig(cfg))

			spec := ProvisionSpec{
				RepoRoot: f.repo, Title: "t", Program: tmux.ProgramClaude, Host: "box1",
				CloneURL: "https://example.invalid/fixture.git",
				Account:  sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: f.accountDir},
			}
			_, err := sshRuntime{}.Provision(spec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "backend=ssh")
			assert.Contains(t, err.Error(), `"work"`, "the actionable message names the account, as the local path does")
			assert.NotContains(t, err.Error(), "dial", "the host must not have been contacted")
		})
	}

	// Control: the bare agent name passes the shape check (provisioning then fails
	// later, on the unreachable host — not on the account).
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	require.NoError(t, config.SaveConfig(cfg))
	spec := ProvisionSpec{
		RepoRoot: f.repo, Title: "t", Program: tmux.ProgramClaude, Host: "box1",
		CloneURL: "https://example.invalid/fixture.git",
		Account:  sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: f.accountDir},
	}
	require.NoError(t, validateRemoteAccountProgram(mustResolve(t, f.repo), &spec))
}

func mustResolve(t *testing.T, repo string) *config.ResolvedConfig {
	t.Helper()
	cfg, err := resolveRepoConfig(repo)
	require.NoError(t, err)
	return cfg
}

// A program override that changed since the session was created is refused in the
// reprovision PREFLIGHT, alongside the persisted-account checks: the recoverable
// runtime must be left intact, not reaped and then refused.
func TestRestoreRefusesAChangedUnprovenCommandBeforeReaping(t *testing.T) {
	f := newListedFixture(t, oneListedHost)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "CLAUDE_CONFIG_DIR=/other claude"}
	require.NoError(t, config.SaveConfig(cfg))

	reaped := false
	i := &Instance{
		Title: "s", Path: f.repo, Branch: "root/s", Host: "box1", Program: tmux.ProgramClaude,
		Account: "work", backend: newInertSandboxBackend("ssh"),
		runtimeTeardown: func() error { reaped = true; return nil },
	}
	err := i.reprovisionRemote()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"work"`)
	assert.False(t, reaped, "the existing runtime and workspace must survive a deterministic refusal")
	assert.Empty(t, f.spec.RepoRoot, "and nothing was provisioned")
}

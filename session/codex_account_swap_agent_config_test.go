package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// codexCredFile is codex's credential file (accountCredentialRel[codex]).
const codexCredFile = "auth.json"

// codexSwapServer is the codex analogue of acctSwapServer: it runs the REAL
// scope rotation for codex (newScopeRotation(ProgramCodex) reads CODEX_HOME) on
// the host directory. acctSwapServer.swap() is hardcoded to claude, so a codex
// E2E swap needs this bespoke helper. scope is the host's live account scope,
// restored for the rotation (newScopeRotation derives the live scope from
// CODEX_HOME and requires it to be a session ".../account" path).
type codexSwapServer struct {
	AgentServer
	live, next, scope string
	can               bool
	rec               accountSwapRecorder
}

func (s *codexSwapServer) CanSwapAccount() (bool, error) { return s.can, nil }
func (s *codexSwapServer) AccountSwapStatus() (string, string, error) {
	o, p := s.rec.status()
	return o, p, nil
}
func (s *codexSwapServer) SwapAccountInPlace() (string, error) {
	return s.rec.run(func() (string, error) {
		os.Setenv("CODEX_HOME", s.scope) // rotate reads the host's live account scope
		rot, err := newScopeRotation(tmux.ProgramCodex)
		if err != nil {
			return "", err
		}
		if err := rot.rotate(); err != nil {
			return "", err
		}
		return "codex", nil
	})
}

// A fresh codex swap stages the operator's config.toml (with the forced file
// credential store) into the incoming scope, so the live scope carries it after
// the rotation. The control for the restored-path regression below.
func TestCodexAgentConfigSurvivesFreshSwap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.SandboxAgentConfig = []string{"codex:config.toml"}
	require.NoError(t, config.SaveConfig(cfg))

	workDir, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramCodex, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, codexCredFile), []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, codexCredFile), []byte(secretAlt), 0o600))

	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")

	dir := t.TempDir()
	p := &sandboxProvisioner{sessionDir: dir}
	p.runCommandFn = localShell{}.Run
	acct := sessionenv.Account{Agent: tmux.ProgramCodex, Name: "work", Dir: workDir}
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{
		Account: acct, AccountSyncHost: "box1", Program: tmux.ProgramCodex,
		// Provision records the operator's selection on the spec; the swap
		// stages it from there, never by re-reading the global config.
		AgentConfigFiles: []string{"codex:config.toml"},
	}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	srv := &codexSwapServer{live: w.AccountDir(), next: w.NextAccountDir(), scope: w.AccountDir(), can: true}
	t.Setenv("CODEX_HOME", w.AccountDir())
	inst := &Instance{
		Title: "codexswap", Path: repo, Program: tmux.ProgramCodex, Host: "box1",
		Account: "work", accountAgent: tmux.ProgramCodex,
		backend: &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir, HostName: "box1"}},
		started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv

	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "config.toml"),
		[]byte(`model = "o3"`+"\n"+`cli_auth_credentials_store = "keyring"`+"\n"), 0o600))
	t.Setenv("CODEX_HOME", opHome)

	_, err = inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	got := readOrEmpty(filepath.Join(dir, "account", "config.toml"))
	assert.Contains(t, got, "cli_auth_credentials_store = 'file'", "file credential store forced through the swap")
	assert.NotContains(t, got, "keyring")
	assert.Equal(t, secretAlt, readOrEmpty(filepath.Join(dir, "account", codexCredFile)), "codex credential swapped in")
}

// The other half of the restored-workspace gate (Codex P2): a session that was
// provisioned with an EMPTY sandbox_agent_config selection is not a restored
// workspace — its spec legitimately carries no files. Enabling the global
// setting afterwards must not push newly selected files onto that old host at
// swap time: the provision-time selection is authoritative for the session's
// life.
func TestCodexFreshSwapKeepsAnEmptyProvisionSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.SandboxAgentConfig = []string{"codex:config.toml"} // enabled AFTER provision
	require.NoError(t, config.SaveConfig(cfg))

	workDir, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramCodex, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, codexCredFile), []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, codexCredFile), []byte(secretAlt), 0o600))

	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")

	dir := t.TempDir()
	p := &sandboxProvisioner{sessionDir: dir}
	p.runCommandFn = localShell{}.Run
	acct := sessionenv.Account{Agent: tmux.ProgramCodex, Name: "work", Dir: workDir}
	// Provision ran while the global selection was empty: no files on the spec
	// and restored=false — indistinguishable from a restored workspace before
	// the restored flag existed, which is the conflation being tested here.
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1", Program: tmux.ProgramCodex}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	srv := &codexSwapServer{live: w.AccountDir(), next: w.NextAccountDir(), scope: w.AccountDir(), can: true}
	t.Setenv("CODEX_HOME", w.AccountDir())
	inst := &Instance{
		Title: "codexswap", Path: repo, Program: tmux.ProgramCodex, Host: "box1",
		Account: "work", accountAgent: tmux.ProgramCodex,
		backend: &sshBackend{provisioner: p, cleanup: &SSHRuntimeCleanupData{SessionDir: dir, HostName: "box1"}},
		started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv

	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "config.toml"),
		[]byte(`model = "o3"`+"\n"+`cli_auth_credentials_store = "keyring"`+"\n"), 0o600))
	t.Setenv("CODEX_HOME", opHome)

	_, err = inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	assert.Empty(t, readOrEmpty(filepath.Join(dir, "account", "config.toml")),
		"a fresh workspace with an empty provision-time selection gains no files from a later global setting")
	assert.Empty(t, p.account.spec.AgentConfigFiles, "the provision-time selection stays empty")
	assert.Equal(t, secretAlt, readOrEmpty(filepath.Join(dir, "account", codexCredFile)), "codex credential still swapped in")
}

// The restore-then-swap regression for codex: a reattached session re-derives
// the operator's config.toml at swap admission, so the swapped-in codex gets the
// forced file credential store (not the keyring default the bug would leave) and
// the rest of the operator's config. The bug report's codex variant: a missing
// config.toml removes the cli_auth_credentials_store = "file" rewrite, leaving
// the swapped-in codex with its built-in default credential-store behaviour
// instead of the pinned file store in the account scope.
func TestCodexAgentConfigSurvivesSwapOnAReattachedSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.SandboxAgentConfig = []string{"codex:config.toml"}
	require.NoError(t, config.SaveConfig(cfg))

	workDir, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	altDir, err := agentaccount.Register(home, tmux.ProgramCodex, "alt")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, codexCredFile), []byte(secretWork), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(altDir, codexCredFile), []byte(secretAlt), 0o600))

	repo := initTempGitRepo(t)
	runGit(t, repo, "remote", "add", "origin", "https://example.invalid/fixture.git")

	dir := t.TempDir()
	p := &sandboxProvisioner{sessionDir: dir}
	p.runCommandFn = localShell{}.Run
	acct := sessionenv.Account{Agent: tmux.ProgramCodex, Name: "work", Dir: workDir}
	w := &sandboxWorkspace{shell: p, SessionDir: dir, spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1", Program: tmux.ProgramCodex}}
	require.NoError(t, w.syncAccountIn(time.Minute))
	p.account = w

	// Simulate the daemon restart: rebuild the provisioner from the persisted
	// cleanup handle + the row's account, exactly as restoreAccountWorkspace does.
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config: config.SSHConfig{Host: "box1.example"}, SessionDir: dir, HostName: "box1",
	}}
	backend, _, err := restoreRuntimeCleanupWithAccount("codexswap", "ssh", data,
		restoredAccount{Name: "work", Agent: tmux.ProgramCodex, Program: tmux.ProgramCodex})
	require.NoError(t, err)
	rp := backend.(*sshBackend).provisioner
	rp.runCommandFn = localShell{}.Run
	rw := rp.account
	require.NotNil(t, rw)
	require.Empty(t, rw.spec.AgentConfigFiles, "restore repopulates no AgentConfigFiles (#5948)")

	srv := &codexSwapServer{live: rw.AccountDir(), next: rw.NextAccountDir(), scope: rw.AccountDir(), can: true}
	t.Setenv("CODEX_HOME", rw.AccountDir())
	inst := &Instance{
		Title: "codexswap", Path: repo, Program: tmux.ProgramCodex, Host: "box1",
		Account: "work", accountAgent: tmux.ProgramCodex,
		backend: backend, started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = srv

	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "config.toml"),
		[]byte(`model = "o3"`+"\n"+`cli_auth_credentials_store = "keyring"`+"\n"), 0o600))
	t.Setenv("CODEX_HOME", opHome)

	_, err = inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	got := readOrEmpty(filepath.Join(dir, "account", "config.toml"))
	require.NotEmpty(t, got, "config.toml was re-derived and staged for the restored swap (bug would leave it empty)")
	tree := map[string]any{}
	require.NoError(t, toml.Unmarshal([]byte(got), &tree), got)
	assert.Equal(t, "file", tree[codexCredStoreKey], "file credential store forced (keyring would re-expose the machine-wide identity)")
	assert.Equal(t, "o3", tree["model"], "the rest of the operator's config is kept")
	assert.NotContains(t, got, "keyring")
	assert.Equal(t, secretAlt, readOrEmpty(filepath.Join(dir, "account", codexCredFile)), "codex credential swapped in")
	assert.Equal(t, []string{"codex:config.toml"}, rp.account.spec.AgentConfigFiles,
		"the re-derived selection is retained on the restored workspace for any later swap")
}

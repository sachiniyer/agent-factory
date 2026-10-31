package session

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #5948: a session reattached after a daemon restart gets its account workspace
// back, rebuilt from the row alone — so account handoff and failover apply to it,
// and nothing is written back to the registry on the strength of a baseline this
// process never had.

// restartedFrom simulates the daemon restart over a swapFixture's host: the
// provisioner is rebuilt from the persisted cleanup handle and the row's account,
// with the host reachable through the fixture's local shell.
func restartedFrom(t *testing.T, f *swapFixture, hostName string) *Instance {
	t.Helper()
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config: config.SSHConfig{Host: "box1.example"}, SessionDir: f.dir, HostName: hostName,
	}}
	backend, _, err := restoreRuntimeCleanupWithAccount("swap", "ssh", data, restoredAccount{Name: "work", Agent: tmux.ProgramClaude, Program: tmux.ProgramClaude})
	require.NoError(t, err)
	p := backend.(*sshBackend).provisioner
	p.runCommandFn = func(d time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		return localShell{}.Run(d, script, stdin, combined)
	}
	inst := &Instance{
		Title: "swap", Path: f.inst.Path, Program: tmux.ProgramClaude, Host: "box1",
		Account: "work", accountAgent: tmux.ProgramClaude,
		backend: backend, started: true, Tabs: []*Tab{newRemoteAgentTab()},
	}
	inst.agentSrv = f.srv
	return inst
}

func TestReattachedSessionRegainsItsAccountWorkspaceFromTheRow(t *testing.T) {
	f := newSwapFixture(t)
	inst := restartedFrom(t, f, "box1")

	assert.True(t, inst.OffBoxAccountHandoffApplies(), "failover is no longer disabled for the reattached session")
	w := inst.sandboxProvisioner().account
	require.NotNil(t, w)
	assert.Equal(t, "work", w.spec.Account.Name)
	assert.Equal(t, filepath.Dir(f.workReg), w.spec.Account.Dir, "the registry directory, resolved locally")
	assert.Equal(t, "box1", w.spec.AccountSyncHost, "the same listed host licenses it")
	assert.Equal(t, f.dir, w.SessionDir)
	assert.Nil(t, w.syncedVersion, "no sync-in baseline")
	assert.Equal(t, secretWork, readOrEmpty(f.workReg), "restoring touched no registry credential")
}

// Without a baseline nothing is written back: a refresh the agent made before the
// restart stays on the host rather than replacing the registry's copy on the
// strength of a version this process never recorded.
func TestRestoredAccountWorkspaceWritesNothingBack(t *testing.T) {
	f := newSwapFixture(t)
	f.refreshOnHost(t, "refreshed-before-restart")
	w := restartedFrom(t, f, "box1").sandboxProvisioner().account

	res, err := w.writeBackAccount(time.Minute)
	require.NoError(t, err)
	assert.Zero(t, res.Written)
	assert.NotEmpty(t, res.Skipped)
	assert.Equal(t, secretWork, readOrEmpty(f.workReg))
}

// The handoff itself works from the restored workspace: B goes live, A's registry
// copy is untouched (no baseline), and B's own baseline is recorded for the
// session's later refreshes.
func TestAccountHandoffRunsOnAReattachedSession(t *testing.T) {
	f := newSwapFixture(t)
	inst := restartedFrom(t, f, "box1")

	res, err := inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)
	assert.Zero(t, res.WrittenBack, "no baseline for A in this process")
	assert.Equal(t, secretWork, readOrEmpty(f.workReg))
	assert.Equal(t, secretAlt, f.hostCred(t, "account"))
	acct, _ := inst.AccountSelection()
	assert.Equal(t, "alt", acct)

	w := inst.sandboxProvisioner().account
	f.refreshOnHost(t, "alt-refreshed") // f.w points at the same host scope
	back, err := w.writeBackAccount(time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, back.Written, "B's baseline was recorded by the swap")
	assert.Equal(t, "alt-refreshed", readOrEmpty(f.altReg))
}

// Nothing is rebuilt without the evidence for it, and the session behaves as
// before: no off-box handoff.
func TestAccountWorkspaceIsNotRestoredWithoutItsEvidence(t *testing.T) {
	f := newSwapFixture(t)
	restore := func(host string, acct restoredAccount) *sandboxProvisioner {
		data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
			Config: config.SSHConfig{Host: "box1.example"}, SessionDir: f.dir, HostName: host,
		}}
		backend, _, err := restoreRuntimeCleanupWithAccount("s", "ssh", data, acct)
		require.NoError(t, err)
		return backend.(*sshBackend).provisioner
	}
	ok := restoredAccount{Name: "work", Agent: tmux.ProgramClaude, Program: tmux.ProgramClaude}
	assert.NotNil(t, restore("box1", ok).account, "control")
	assert.Nil(t, restore("", ok).account, "no listed host recorded: no licence")
	assert.Nil(t, restore("box1", restoredAccount{Agent: tmux.ProgramClaude}).account, "no account on the row")
	assert.Nil(t, restore("box1", restoredAccount{Name: "work"}).account, "no agent on the row")
	assert.Nil(t, restore("box1", restoredAccount{Name: "gone", Agent: tmux.ProgramClaude}).account, "an account the registry no longer has")
	require.NoError(t, os.RemoveAll(filepath.Dir(f.workReg)))
	assert.Nil(t, restore("box1", ok).account, "a registry directory that is gone")
}

// The sandbox backend records the licensing host in its cleanup handle, and a
// session without an account records none.
func TestSandboxCleanupHandleCarriesTheAccountHost(t *testing.T) {
	f := newSwapFixture(t)
	assert.Equal(t, "box1", f.inst.sandboxProvisioner().accountSyncHost())
	assert.Empty(t, (&sandboxProvisioner{}).accountSyncHost())

	data := &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh sandbox.invalid", SessionDir: f.dir, AccountSyncHost: "box1",
	}}
	backend, _, err := restoreRuntimeCleanupWithAccount("s", "sandbox", data, restoredAccount{Name: "work", Agent: tmux.ProgramClaude, Program: tmux.ProgramClaude})
	require.NoError(t, err)
	p := backend.(*sandboxBackend).provisioner
	require.NotNil(t, p.account)
	assert.Equal(t, "box1", p.account.spec.AccountSyncHost)
}

// #5948/#6176: a reattached session's workspace is rebuilt from the row alone, so
// it carries no provision-time spec.AgentConfigFiles — the operator's
// sandbox_agent_config selection. An off-box swap would stage the incoming
// account WITHOUT that personalization (a bare agent) and then rotate away the
// outgoing scope that held the provision-time copy. The swap re-derives the
// selection from the repo config — exactly the closed, repo-tamper-safe
// sandbox_agent_config key, licence-recorded by the durable AccountSyncHost — so
// the swapped-in account runs with the operator's model settings and
// instructions, as it did at provision.
func TestAgentConfigSurvivesAnOffBoxAccountSwapOnAReattachedSession(t *testing.T) {
	f := newSwapFixture(t)
	// The operator's global sandbox_agent_config — the closed, repo-tamper-safe
	// selection provision copies into spec.AgentConfigFiles. restoreAccountWorkspace
	// has no repo config to copy it from, so the reattached session re-derives it
	// at swap admission instead.
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.SandboxAgentConfig = []string{"claude:settings.json", "claude:CLAUDE.md"}
	require.NoError(t, config.SaveConfig(cfg))

	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{"model":"opus"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "CLAUDE.md"), []byte("be terse"), 0o600))

	inst := restartedFrom(t, f, "box1")
	w := inst.sandboxProvisioner().account
	require.NotNil(t, w)
	assert.Empty(t, w.spec.AgentConfigFiles, "restoreAccountWorkspace repopulates no AgentConfigFiles from the row (#5948)")

	// This process is both the daemon box (reads the operator home) and the sandbox
	// (rotates the live scope): the operator home is CLAUDE_CONFIG_DIR while af
	// stages, the host's live scope while the (in-process) agent-server rotates.
	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	inst.agentSrv = hostEnvServer{f.srv, t, hostScope}

	_, err := inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the new account is live")
	assert.Equal(t, `{"model":"opus"}`, readOrEmpty(filepath.Join(f.dir, "account", "settings.json")),
		"the operator's model settings were re-derived and staged for the swapped-in account")
	assert.Equal(t, "be terse", readOrEmpty(filepath.Join(f.dir, "account", "CLAUDE.md")),
		"and the instructions")
	assert.Equal(t, []string{"claude:settings.json", "claude:CLAUDE.md"},
		inst.sandboxProvisioner().account.spec.AgentConfigFiles,
		"the re-derived selection is retained on the workspace for any later swap")
}

// A reattached session whose operator declared no sandbox_agent_config gets none,
// as before: the re-derivation mirrors provision, which sets AgentConfigFiles
// only from the operator's global selection.
func TestNoAgentConfigIsStagedOnAReattachedSessionWhenNoneIsDeclared(t *testing.T) {
	f := newSwapFixture(t)
	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{"model":"opus"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "CLAUDE.md"), []byte("be terse"), 0o600))

	inst := restartedFrom(t, f, "box1")
	w := inst.sandboxProvisioner().account
	require.NotNil(t, w)
	assert.Empty(t, w.spec.AgentConfigFiles, "the reattached session carries no provision-time selection")

	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	inst.agentSrv = hostEnvServer{f.srv, t, hostScope}

	_, err := inst.SwapOffBoxAccount("alt", false)
	require.NoError(t, err)

	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the new account is live")
	assert.NoFileExists(t, filepath.Join(f.dir, "account", "settings.json"), "no operator config was declared to carry")
	assert.NoFileExists(t, filepath.Join(f.dir, "account", "CLAUDE.md"))
	assert.Empty(t, inst.sandboxProvisioner().account.spec.AgentConfigFiles,
		"no selection is fabricated when the operator declared none")
}

// The re-derivation is gated on the durable account-sync licence, exactly as
// provision pairs AgentConfigFiles with AccountSyncHost: a reattached session
// that was never licensed to sync an account (no recorded host) is not handed
// one by the swap.
func TestAgentConfigIsNotReDerivedForAReattachedSessionWithoutAnAccountLicence(t *testing.T) {
	f := newSwapFixture(t)
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	cfg.SandboxAgentConfig = []string{"claude:settings.json"}
	require.NoError(t, config.SaveConfig(cfg))
	opHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opHome, "settings.json"), []byte(`{}`), 0o600))

	inst := restartedFrom(t, f, "box1")
	inst.sandboxProvisioner().account.spec.AccountSyncHost = "" // unlicensed: no recorded host
	hostScope := os.Getenv("CLAUDE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", opHome)
	inst.agentSrv = hostEnvServer{f.srv, t, hostScope}

	// Without the licence the swap is refused before staging, so no operator config
	// is read or sent — the re-derivation does not reach past the gate.
	_, err := inst.SwapOffBoxAccount("alt", false)
	require.ErrorContains(t, err, "not synced")
	assert.NoFileExists(t, filepath.Join(f.dir, "account", "settings.json"))
}

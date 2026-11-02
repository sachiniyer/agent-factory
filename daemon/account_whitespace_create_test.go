package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestCreateRPC_WhitespaceAccountIsNormalizedAtCreateBoundary exercises the real
// daemon control-socket round-trip the bug report describes: a whitespace-only
// --account (e.g. `--account " "`) is trimmed at the RPC create boundary
// (controlServer.createSession, mirroring trimProgramEnumIfBare for Program)
// and again in NewInstance, so the stored Instance.Account is "" (ambient) rather
// than the raw whitespace string. Before the fix, the gates' TrimSpace tests
// treated " " as absent while the record stored it raw — the session ran (or
// failed) with no account scoping while every display surface reported it as
// account-scoped.
func TestCreateRPC_WhitespaceAccountIsNormalizedAtCreateBoundary(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	closeServer, err := startControlServer(manager, nil, nil, make(chan struct{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	// Create via the actual control-socket RPC (net/rpc over the daemon unix
	// socket), so the account flows through controlServer.createSession (which
	// trims) -> Manager.CreateSession -> NewInstance (which normalizes) ->
	// i.Account.
	const title = "ws-account-create"
	var resp CreateSessionResponse
	require.NoError(t, callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Account:  " ",
	}, &resp), "a whitespace-only account must be admitted as ambient, not refused")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, title)]
	require.True(t, ok, "the created session must be registered in the manager")
	require.Empty(t, inst.Account,
		"the daemon create path must trim a whitespace-only account into i.Account — "+
			"the record must not claim an account was selected for an ambient session")
	require.Empty(t, inst.AccountAgent(),
		"no account agent is recorded for an ambient session")
}

// TestCreateRPC_ValidAccountWithSurroundingWhitespaceIsTrimmed verifies that a
// valid account name with surrounding whitespace is trimmed at the create
// boundary and stored as the trimmed name, so the recorded Account is always a
// ValidateName-valid name — never a raw string with surrounding whitespace.
func TestCreateRPC_ValidAccountWithSurroundingWhitespaceIsTrimmed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	closeServer, err := startControlServer(manager, nil, nil, make(chan struct{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	const title = "ws-account-trim"
	var resp CreateSessionResponse
	require.NoError(t, callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Account:  "  work  ",
	}, &resp), "a valid account with surrounding whitespace must be admitted (as trimmed)")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, title)]
	require.True(t, ok, "the created session must be registered in the manager")
	require.Equal(t, "work", inst.Account,
		"the daemon create path must trim surrounding whitespace into i.Account — "+
			"the recorded Account must be the trimmed name, not the raw string")
	require.Equal(t, tmux.ProgramClaude, inst.AccountAgent(),
		"the account agent must be recorded for a scoped session")
}

// TestManagerCreateSession_WhitespaceAccountNormalizedDirectly verifies the
// Manager.CreateSession path (not the RPC boundary) also normalizes a
// whitespace-only account, so direct callers — the root-agent ensure loop, task
// delivery — that bypass controlServer.createSession still get the invariant.
func TestManagerCreateSession_WhitespaceAccountNormalizedDirectly(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Manager.CreateSession does not trim req.Account (the RPC boundary does);
	// NewInstance normalizes it. A whitespace-only account is trimmed to "" by
	// NewInstance, so the stored Instance.Account is "" (ambient).
	data, err := manager.CreateSession(nil, CreateSessionRequest{
		Title:    "ws-manager-direct",
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Account:  "  ",
	})
	require.NoError(t, err, "a whitespace-only account must be admitted as ambient")
	require.Empty(t, data.Account,
		"the returned InstanceData must not claim an account for an ambient session")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, "ws-manager-direct")]
	require.True(t, ok)
	require.Empty(t, inst.Account,
		"the stored Instance must have an empty Account for a whitespace-only request")
}

// TestValidateCreateFirst_WhitespaceAccountTreatedAsAbsent verifies the remote
// pre-check (validateCreateFirst, opt-in via ValidateFirst) treats a
// whitespace-only account as absent rather than reaching agentaccount.Selected
// — which trims internally and silently returns a zero Account for whitespace,
// skipping the validation a non-empty value must pass.
func TestValidateCreateFirst_WhitespaceAccountTreatedAsAbsent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	repoPath := setupControlRepo(t)

	// A docker backend avoids the local-prereqs PATH check (claude on PATH) so
	// the account pre-check is the only thing exercised here.
	docker := CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  "docker",
	}

	// A whitespace-only account must not reach agentaccount.Selected (which
	// would silently succeed); it must be treated as "no account" and skip the
	// pre-check entirely.
	ws := docker
	ws.Account = "   "
	require.NoError(t, validateCreateFirst(ws),
		"a whitespace-only account must be treated as absent by validateCreateFirst")

	// A non-empty invalid name must still be refused by the pre-check, so the
	// fail-closed rule holds for every invalid account input.
	invalid := docker
	invalid.Account = "work!"
	err := validateCreateFirst(invalid)
	require.Error(t, err,
		"a non-empty invalid account name must be refused by validateCreateFirst")
}

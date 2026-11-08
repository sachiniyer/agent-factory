package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the daemon-side host-pick pre-reservation validation. validateCreateFirst
// must refuse an unlisted --host BEFORE reserveCreate can rename an archived session
// holding the requested title, mirroring the client-side localCreateInputs refusal
// (#5744, #2415). Before the fix, the host pick was validated only inside
// sshRuntime.Provision — strictly after renameArchivedForReuseLocked had relocated
// the archived worktree, re-keyed the manager map, and rewritten instances.json — so
// a remote create with a one-character host typo permanently renamed the archive.

// TestValidateCreateFirst_RefusesUnlistedHost is the direct, hermetic unit test: with
// no hosts in the daemon's global list, a remote create's --host value is refused by
// validateCreateFirst before reserveCreate is reached. The refusal is the shared
// config.ValidateHostPick message (identical to localCreateInputs client-side), NOT
// the "backend=ssh:"-wrapped provision-time refusal — the absence of the wrapper is
// what proves the check fired ahead of the rename.
func TestValidateCreateFirst_RefusesUnlistedHost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	repoPath := setupControlRepo(t)

	err := validateCreateFirst(CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  config.BackendSSH,
		Host:     "no-such-host",
	})
	require.Error(t, err, "an unlisted host must be refused before the reservation")
	assert.Contains(t, err.Error(), `host "no-such-host" is not in the host list`,
		"the refusal must be the shared ValidateHostPick message, identical to localCreateInputs client-side")
	// The provision-time refusal wraps ValidateHostPick with "backend=ssh:"; the
	// pre-reservation refusal does not. The absence of the wrapper is what proves the
	// check fired at validateCreateFirst rather than at sshRuntime.Provision.
	assert.NotContains(t, err.Error(), "backend=ssh:",
		"the refusal must come from the pre-reservation hook, not post-rename provisioning")
}

// TestValidateCreateFirst_AdmitsListedAndEmptyHost locks in the no-regression
// direction: a listed host and an absent pick are both admitted, so the new check
// fires only for the unlisted-host class the bug is about and leaves every valid
// create (local, ssh with a listed host, ssh with no pick) untouched.
func TestValidateCreateFirst_AdmitsListedAndEmptyHost(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.Hosts = map[string]config.HostConfig{"build-box": {Host: "build-box.example"}}
	require.NoError(t, config.SaveConfig(cfg))
	repoPath := setupControlRepo(t)

	// A listed host pick is admitted: the operator's global list contains it, so the
	// check returns nil and the create proceeds to reserveCreate exactly as before.
	require.NoError(t, validateCreateFirst(CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  config.BackendSSH,
		Host:     "build-box",
	}), "a listed host must be admitted; the check only refuses unlisted hosts")

	// An empty host is "no pick" and must never be refused, for any backend — this is
	// ValidateHostPick's own contract and the reason the check is skipped for an
	// an empty value rather than run against it.
	require.NoError(t, validateCreateFirst(CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  config.BackendSSH,
		Host:     "",
	}), "an empty host is 'no pick' and must never be refused")
	require.NoError(t, validateCreateFirst(CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  "docker",
		Host:     "",
	}), "an empty host is 'no pick' and must never be refused")
}

// TestCreateRPC_InvalidHostRefusesBeforeArchivedRename is the headline end-to-end
// reproduction via the real control-socket RPC (net/rpc over the daemon unix socket).
// It seeds an archived session titled "shared", then issues CreateSession with
// Title "shared", Backend ssh, Host "no-such-host", ValidateFirst true — the exact
// shape of `af sessions create shared --host typo --daemon-url <remote>` against a
// daemon whose host list is empty.
//
// Before the fix the create still errored (provision-time chooseListedHost rejected
// the host) but only AFTER reserveCreate had renamed the archived session to
// "shared (archived)": worktree physically relocated, manager map re-keyed,
// instances.json rewritten, EventSessionUpdated published — permanent state
// corruption from a one-character CLI typo, with no rollback. After the fix the
// refusal lands in validateCreateFirst, ahead of the reservation, and the archived
// session is left untouched on every surface the rename would have mutated.
func TestCreateRPC_InvalidHostRefusesBeforeArchivedRename(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// Empty host list: any non-empty pick is unlisted, matching the bug report's
	// `--host typo` against a daemon with no hosts configured.
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	manager, repoID, repoPath := newStatusTestManager(t)
	// Seed an archived session titled "shared" with its branch freed, so #2127's
	// held-branch guard cannot refuse the create first: the only refusal left is the
	// host-pick membership check this test is about. This is the shape in which
	// reuse-archived-name can actually complete (the rename would fire absent the fix).
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")

	closeServer, err := startControlServer(manager, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	// The exact remote-create shape: ValidateFirst is what routedCreateSession sets
	// for --daemon-url, Backend ssh with a listed-host pick, title colliding with the
	// archived row. Hermetic — it fails at validateCreateFirst before any ssh dialing.
	var resp CreateSessionResponse
	createErr := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:         "shared",
		RepoPath:      repoPath,
		Program:       tmux.ProgramClaude,
		Backend:       config.BackendSSH,
		Host:          "no-such-host",
		ValidateFirst: true,
	}, &resp)
	require.Error(t, createErr, "the create must be refused: the host is not in the daemon's list")
	assert.Contains(t, createErr.Error(), `host "no-such-host" is not in the host list`,
		"the refusal must be the shared ValidateHostPick message")
	// The provision-time refusal wraps ValidateHostPick with "backend=ssh:". Its
	// absence here proves the refusal came from the pre-reservation hook — before the
	// rename — rather than after the archived session was relocated, which is the
	// distinction the bug report draws between the fixed and unfixed paths.
	assert.NotContains(t, createErr.Error(), "backend=ssh:",
		"the refusal must come from validateCreateFirst, not post-rename provisioning")
	assert.Empty(t, resp.Instance.ID,
		"no session may be committed for a create refused before the reservation")

	// The archived session must be intact on every surface the rename would have
	// touched: title, manager key, durable record, worktree path, and restorability.
	// assertArchivedUntouched pins all of them together so a fix that reverted only
	// some would still fail the test.
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

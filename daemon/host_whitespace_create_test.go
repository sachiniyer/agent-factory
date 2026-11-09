package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the daemon-side host-pick whitespace normalization. controlServer.
// createSession must trim a whitespace-only --host value at the RPC boundary
// (mirroring the Account trim and the validateCreateFirst gate's own TrimSpace
// reading), so every downstream stage of the create pipeline — backendSelection,
// resolveBackendKind, InPlaceBackendConflict, chooseListedHost, recordedAccountHost
// — reads the SAME value the validator did. Before the fix, validateCreateFirst
// trimmed a LOCAL copy to decide the host-list check was skippable (whitespace =
// "absent"), while the rest of the pipeline read the RAW req.Host and treated
// whitespace as a real ssh pick: resolveBackendKind forced BackendSSH, the durable
// Instance.Host recorded " " verbatim, and chooseListedHost trimmed it right back
// to "no pick" — so a remote create with --host " " (the CLI default Backend=""
// path, e.g. via an env var that collapses to whitespace) was admitted, renamed an
// archived session holding the title to free the name, and failed only inside
// sshRuntime.Provision (after the rename), reopening the fail-after-mutation class
// #5744/#6988 the validateCreateFirst host gate was added to close. The Account
// twin of this bug was fixed across the pipeline (#6605, regression-tested in
// account_whitespace_create_test.go); the Host field got the boundary trim here.

// TestCreateRPC_WhitespaceHostIsNormalizedAtCreateBoundary exercises the real
// daemon control-socket round-trip the bug report describes: a whitespace-only
// --host (e.g. `--host " "`) is trimmed at the RPC create boundary
// (controlServer.createSession, mirroring the Account trim), so the stored
// Instance.Host is "" (no pick) rather than the raw whitespace string. Before the
// fix, validateCreateFirst treated " " as absent (its TrimSpace gate skipped the
// host-list check) while every downstream stage treated it as a real ssh pick — so
// resolveBackendKind forced BackendSSH and recordedAccountHost recorded " "
// verbatim on the durable Instance, a "present but empty" host that blanks every
// p.Host != "" gate (session/placement.go, api/tasks.go) and defeats the
// recorded-resolved-host pinning invariant for account sessions, with no error.
//
// Uses the options-recording FakeBackend so no real tmux/git/ssh provisioning runs:
// the assertion is about the recorded field, not the runtime. ValidateFirst is
// deliberately NOT set so validateCreateFirst (and its LocalSessionPrereqsAt PATH
// check) does not run — mirroring TestCreateRPC_WhitespaceAccountIsNormalizedAtCreateBoundary.
func TestCreateRPC_WhitespaceHostIsNormalizedAtCreateBoundary(t *testing.T) {
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
	// socket), so the host flows through controlServer.createSession (which trims)
	// -> Manager.CreateSession -> NewInstance -> recordedAccountHost -> i.Host.
	// Backend is empty (the CLI default for `--host` without `--background`); the
	// FakeBackend ignores the resolved kind, so the create succeeds for any kind.
	const title = "ws-host-create"
	var resp CreateSessionResponse
	require.NoError(t, callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Host:     " ",
	}, &resp), "a whitespace-only host must be admitted as no-pick, not refused")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, title)]
	require.True(t, ok, "the created session must be registered in the manager")
	require.Empty(t, inst.Host,
		"the daemon create path must trim a whitespace-only host into i.Host — "+
			"the durable record must not carry a 'present but empty' host that "+
			"blanks every p.Host != \"\" display gate and defeats the resolved-host pin")
}

// TestCreateRPC_ValidHostWithSurroundingWhitespaceIsTrimmed verifies that a
// listed host name with surrounding whitespace is trimmed at the create boundary
// and stored as the trimmed name, so the recorded Instance.Host is always the
// pick as named — never a raw string with surrounding whitespace that
// ValidateHostPick's exact-map-key lookup would later refuse. Mirrors
// TestCreateRPC_ValidAccountWithSurroundingWhitespaceIsTrimmed.
func TestCreateRPC_ValidHostWithSurroundingWhitespaceIsTrimmed(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.Hosts = map[string]config.HostConfig{"build-box": {Host: "build-box.example"}}
	require.NoError(t, config.SaveConfig(cfg))

	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	closeServer, err := startControlServer(manager, nil, nil, make(chan struct{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	const title = "ws-host-trim"
	var resp CreateSessionResponse
	require.NoError(t, callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Host:     "  build-box  ",
	}, &resp), "a listed host with surrounding whitespace must be admitted (as trimmed)")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, title)]
	require.True(t, ok, "the created session must be registered in the manager")
	require.Equal(t, "build-box", inst.Host,
		"the daemon create path must trim surrounding whitespace into i.Host — "+
			"the recorded Host must be the named pick, not the raw string")
}

// TestCreateRPC_WhitespaceHostDoesNotFailAfterArchivedRename is the headline
// end-to-end reproduction via the real control-socket RPC, mirroring
// TestCreateRPC_InvalidHostRefusesBeforeArchivedRename for the whitespace shape.
// It seeds an archived session titled "shared", then issues CreateSession with
// Title "shared", Backend "" (the CLI default for `--host` without `--background`),
// Host " ", ValidateFirst true — the exact shape of `af sessions create shared
// --host " "` (e.g. via an env var that collapses to whitespace) over --daemon-url
// against a daemon whose host list is empty and whose repo has no [ssh] section.
//
// Before the fix the whitespace pick was trimmed to "no pick" ONLY inside
// validateCreateFirst's host-list gate (so the gate skipped), but the RAW " "
// flowed downstream: resolveBackendKind saw opts.Host != "" and forced BackendSSH,
// createNeedsDaemonHostPrereqs reported local=false (so the local-prereqs PATH
// check was skipped too), validateCreateFirst returned nil, reserveCreate renamed
// the archived session "shared" to "shared (archived)" (worktree relocated, manager
// map re-keyed, instances.json rewritten — no rollback), and only then did
// sshRuntime.Provision's chooseListedHost trim " " back to "no pick" and fail with
// "backend=ssh requires ssh.host to be set …" — strictly AFTER the rename.
//
// After the fix the boundary trim makes req.Host "" before validateCreateFirst, so
// resolveBackendKind resolves from config (default local) rather than being forced
// to ssh by the whitespace pick: createNeedsDaemonHostPrereqs reports local=true,
// and LocalSessionPrereqsAt runs BEFORE reserveCreate, refusing the create (the
// agent CLI is not on this daemon's PATH) ahead of the reservation — so the
// archived session is left untouched on every surface the rename would have
// mutated, and the refusal is NOT the post-rename "requires ssh.host" message.
//
// The assumption that the agent CLI (claude) is not on the daemon host's PATH is
// the same one account_whitespace_create_test.go relies on (it uses a docker
// backend to AVOID this check); it holds in this sandbox and in CI.
func TestCreateRPC_WhitespaceHostDoesNotFailAfterArchivedRename(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// Empty host list, no repo [ssh] section, no default_host: a non-empty pick is
	// unlisted and a whitespace pick collapses to "no pick", matching the bug
	// report's `--host " "` against a daemon with no ssh target configured.
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	manager, repoID, repoPath := newStatusTestManager(t)
	// Seed an archived session titled "shared" with its branch freed, so #2127's
	// held-branch guard cannot refuse the create first: the only refusal left is
	// the one this test is about (the whitespace host-pick shape). This is the
	// shape in which reuse-archived-name can actually complete (the rename would
	// fire absent the fix).
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")

	closeServer, err := startControlServer(manager, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	// The exact remote-create shape: ValidateFirst is what routedCreateSession sets
	// for --daemon-url, Backend "" is the CLI default for `--host` without
	// `--background`, title colliding with the archived row. Hermetic — after the
	// fix it is refused at validateCreateFirst's LocalSessionPrereqsAt (claude not
	// on PATH) before any ssh dialing or archived rename.
	var resp CreateSessionResponse
	createErr := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:         "shared",
		RepoPath:      repoPath,
		Program:       tmux.ProgramClaude,
		Host:          " ",
		ValidateFirst: true,
	}, &resp)
	require.Error(t, createErr,
		"the create must be refused: a whitespace host must not place the session on ssh "+
			"and silently rename an archived session for a create that cannot run")
	// The provision-time refusal is "backend=ssh requires ssh.host to be set …".
	// Its absence proves the create never reached sshRuntime.Provision — i.e. the
	// whitespace pick did not force BackendSSH and drag the create through
	// reserveCreate into a post-rename ssh failure. This is the discriminator
	// between the fixed path (pre-rename refusal on the resolved local backend)
	// and the unfixed path (post-rename ssh-provision failure).
	assert.NotContains(t, createErr.Error(), "requires ssh.host to be set",
		"the refusal must NOT be the post-rename ssh-provision message — the "+
			"whitespace pick must not force BackendSSH and reach sshRuntime.Provision")
	assert.Empty(t, resp.Instance.ID,
		"no session may be committed for a create refused before the reservation")

	// The archived session must be intact on every surface the rename would have
	// touched: title, manager key, durable record, worktree path, and restorability.
	// assertArchivedUntouched pins all of them together so a fix that reverted only
	// some would still fail the test. Before the fix this helper failed on every
	// check (the archive was renamed to "shared (archived)" for a create that
	// never happened).
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

// TestValidateCreateFirst_WhitespaceHostTreatedAsAbsent verifies the remote
// pre-check (validateCreateFirst, opt-in via ValidateFirst) treats a
// whitespace-only host as absent — its OWN TrimSpace gate skips the host-list
// check for one — so a whitespace pick is never refused as "not in the host list".
// This locks in the validator's "absent" reading of whitespace, which is WHY the
// boundary trim (treating whitespace as no-pick everywhere) is the right
// unification rather than refusing whitespace in the validator. A non-empty
// unlisted pick is still refused, so the fail-closed rule holds for every invalid
// host input. Mirrors TestValidateCreateFirst_WhitespaceAccountTreatedAsAbsent.
//
// A docker backend would contradict a non-empty Host pick (resolveBackendKind
// refuses --host + --backend docker); an explicit ssh backend avoids both the
// contradiction and the LocalSessionPrereqsAt PATH check (ssh is not local), so
// the host-list gate is the only thing exercised here.
func TestValidateCreateFirst_WhitespaceHostTreatedAsAbsent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	repoPath := setupControlRepo(t)

	ssh := CreateSessionRequest{
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
		Backend:  config.BackendSSH,
	}

	// A whitespace-only host must not reach ValidateHostPick as a non-empty name
	// (which would refuse it as unlisted); it must be treated as "no pick" and
	// skip the host-list pre-check entirely.
	ws := ssh
	ws.Host = "   "
	require.NoError(t, validateCreateFirst(ws),
		"a whitespace-only host must be treated as absent by validateCreateFirst")

	// A non-empty unlisted host must still be refused by the pre-check, so the
	// fail-closed rule holds for every invalid host input.
	invalid := ssh
	invalid.Host = "no-such-host"
	err := validateCreateFirst(invalid)
	require.Error(t, err,
		"a non-empty unlisted host must be refused by validateCreateFirst")
	assert.Contains(t, err.Error(), `host "no-such-host" is not in the host list`,
		"the refusal must be the shared ValidateHostPick message, identical to localCreateInputs client-side")
}

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the daemon-side unresolvable-config pre-reservation admission.
// reserveCreate (or its delegate validateCreateFirst on the ValidateFirst path)
// must refuse a create whose in-repo project config exists but cannot be resolved
// BEFORE renameArchivedForReuseLocked can rename an archived session that held
// the requested title, mirroring the unlisted --host hoist (#5744/#6988, #2415).
//
// Before the fix the #5801/#7078 refusal was deliberately swallowed in
// reserveCreate (its createBackendKind error was dropped) and surfaced only at
// session.NewInstance — strictly after the archived session had been renamed to
// "<title> (archived)", its worktree relocated, its branch moved, and its durable
// record re-keyed — for a create that can never succeed, with no rollback. The
// renamed archive could no longer be looked up by its original title (only by
// the new title or stable id), and every retry with the same title failed
// identically. The same shape existed for an unusable `backend` value, where the
// canonical refusal at NewInstance also used to land after the rename.

// writeBrokenInRepoConfig writes an unparseable `.agent-factory/config.toml` to
// the repo root: the exact shape the report names ("this is [[not toml"). The
// file's presence forces resolveBackendKind off its "no in-repo config" default
// and into the parsed-but-broken arm of resolveProjectConfig, which is what
// surfaces the #5801 refusal rather than falling back to local.
func writeBrokenInRepoConfig(t *testing.T, repoPath, body string) {
	t.Helper()
	dir := filepath.Join(repoPath, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644))
}

// TestReserveCreate_UnresolvableConfigRefusesBeforeArchivedRename is the headline
// reproduction: an archived "shared" plus a broken `.agent-factory/config.toml`
// plus a create with Title "shared". Before the fix this relocated/re-keyed the
// archived session to "shared (archived)" and THEN refused at NewInstance. The
// archived session was permanently renamed, with no rollback, for a create that
// never happened, and a restore addressed by the original title no longer found
// it. After the fix the refusal lands inside reserveCreate, ahead of the archived
// rename, and the archived session is untouched on every surface the rename
// would have mutated.
func TestReserveCreate_UnresolvableConfigRefusesBeforeArchivedRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Seed an archived "shared" with its branch already freed, so #2127's
	// held-branch guard cannot refuse the create first: the only refusal left
	// is the unresolvable-config class this test is about, which is the shape
	// in which the rename would actually fire absent the fix.
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")
	// Broken TOML in the repo's in-repo config: resolveProjectConfig refuses
	// (config/inrepo.go's "failed to parse in-repo config"), and
	// resolveBackendKind wraps it with the #5801 isolation-bypass message.
	writeBrokenInRepoConfig(t, repoPath, "this is [[not toml")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "shared",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: the repo's in-repo config cannot be resolved")
	assert.Contains(t, err.Error(), "cannot determine the backend for",
		"the refusal must be the #5801 backend-resolution refusal, not a generic create failure")
	assert.Contains(t, err.Error(), "failed to parse in-repo config",
		"the refusal must name the broken in-repo config so the user can find the cause")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	// The archived session must be intact on every surface the rename would
	// have touched: title, manager key, durable record, worktree path, and
	// restorability — assertArchivedUntouched pins all of them together so a
	// fix that reverted only some would still fail the test.
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

// TestReserveCreate_InvalidBackendValueRefusesBeforeArchivedRename covers the
// OTHER class createBackendKind refuses for — an unusable `backend` VALUE, the
// case 48afb0e2/#2806 originally wrote the swallowed-kerr contract for. It is
// the same ordering defect as the unresolvable-config case: the canonical
// refusal lived at NewInstance, which runs after the archived rename, so a
// create with an explicit title held only by an archived session renamed it and
// then failed. The hoist refuses the invalid value ahead of the rename too.
//
// No broken config file is needed: the explicit Backend value is itself the
// refusal trigger, regardless of the repo's config (ParseBackendKind fails
// before resolveProjectConfig is even consulted, since opts.Backend != "").
func TestReserveCreate_InvalidBackendValueRefusesBeforeArchivedRename(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "shared",
		Program:  "claude",
		Backend:  "not-a-backend",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: the requested backend value is not a backend")
	assert.Contains(t, err.Error(), "not-a-backend",
		"the refusal must name the unusable value the user typed")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

// TestReserveCreate_UnresolvableConfigWithoutArchivedCollisionStillRefuses keeps
// the hoist narrow: a create against an unresolvable repo config is refused
// regardless of any archived collision, but with no archived collision there is
// no rename to protect — the refusal must land with the same canonical message
// from reserveCreate rather than be swallowed and re-emerge at NewInstance. The
// pre-fix behavior here also reached NewInstance's refusal (no rename fired
// because there was no archived session under the title), so this case is a
// no-regression guard: the hoist does not change the verdict for a create that
// reserveCreate previously refused — it just keeps the message in the same
// canonical shape and refuses a step earlier.
func TestReserveCreate_UnresolvableConfigWithoutArchivedCollisionStillRefuses(t *testing.T) {
	manager, _, repoPath := newStatusTestManager(t)
	writeBrokenInRepoConfig(t, repoPath, "this is [[not toml")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "fresh-title",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "without an in-repo-defaultable backend the create must be refused")
	assert.Contains(t, err.Error(), "cannot determine the backend for", "the canonical #5801 refusal fires")
	assert.Contains(t, err.Error(), "failed to parse in-repo config",
		"the refusal must point at the broken in-repo config, the actionable cause")
	assert.Nil(t, renamed, "no archived rename may be reported for a create without an archived collision")
}

// TestReserveCreate_ResolvedConfigStillAdmitsReuse is the no-regression
// direction for the hoist: a repo whose in-repo config resolves cleanly to a
// resolvable backend continues to admit reuse of an archived-session title. The
// hoist only refuses the unresolvable class; a fully-resolved config — even one
// selecting an off-box backend — must still pass the hoist so the rename
// machinery can free the name for the new session.
func TestReserveCreate_ResolvedConfigStillAdmitsReuse(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")

	// A valid in-repo config selecting the local backend: createBackendKind
	// succeeds, the hoist is satisfied, and the archived rename proceeds as
	// before. (A non-local backend would reach the in-place/off-box
	// contradiction only when InPlace is set; without --here it would
	// successfully rename the archived session and then run on that backend.)
	require.NoError(t, os.MkdirAll(filepath.Join(repoPath, config.InRepoConfigDirName), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repoPath, config.InRepoConfigDirName, "config.toml"),
		[]byte("backend = \"local\"\n"), 0o644))

	_, title, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "shared",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.NoError(t, err, "a resolvable in-repo config selecting local must admit the reuse")
	assert.Equal(t, "shared", title, "the freed title must be granted verbatim")
	require.NotNil(t, renamed, "the archived session must have been renamed to free the name")
	assert.Equal(t, "shared (archived)", renamed.Title)
	// The original archived row is now keyed under the disambiguated name;
	// sanity-check that the manager map and on-disk record carried it across,
	// which is the post-rename state TestReserveCreate_ReusesArchivedName in
	// create_reuse_archived_test.go already pins in full.
	manager.mu.Lock()
	_, oldKeyed := manager.instances[daemonInstanceKey(repoID, "shared")]
	renamedInst, newKeyed := manager.instances[daemonInstanceKey(repoID, "shared (archived)")]
	manager.mu.Unlock()
	assert.False(t, oldKeyed, "the archived row must no longer be keyed under the freed name")
	assert.True(t, newKeyed, "the archived row must be keyed under its new name")
	assert.Same(t, archived, renamedInst, "re-keying preserves the same instance object (stable id)")
}

// TestReserveCreate_UnresolvableConfigRefusesAheadOfArchivedRenamedNeighbor
// orders the new refusal ahead of validateTitleAvailableLocked as the report's
// recommended-fix note requires. The mangling here is shaped so the title would
// ALSO be refused post-rename by validateTitleAvailableLocked, which (under the
// unfixed code) led to a doubly-broken state: the archived session was renamed
// onto a name and then the create failed on a record-independent title check
// that the rename has no effect on (#2415's own example, the orphan-tmux slot
// the rename lands on). The hoist makes the unresolvable-config refusal the
// FIRST one the user sees, so they get the actionable "fix your config" message
// rather than a misleading "title conflict" message naming a tmux session the
// rename created (and would have left orphaned, with no rollback).
//
// Concretely: the archived session is seeded as "shared" with a freed branch,
// then a totally separate tmux orphan named "<sanitized-of-shared (archived)>"
// is planted — the exact slot the rename would pick — so the rename would land
// on a name validateTitleAvailableLocked refuses on its record-independent
// orphan-tmux check. Under the hoist the broken-config refusal fires first and
// the orphan is irrelevant; the user never hears about the orphan they would
// have heard about under the bug.
func TestReserveCreate_UnresolvableConfigRefusesAheadOfArchivedRenamedNeighbor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")
	writeBrokenInRepoConfig(t, repoPath, "this is [[not toml")

	// The slot the rename would pick is "<sanitized of 'shared (archived)'>";
	// plant an orphan tmux session there so validateTitleAvailableLocked
	// would refuse AFTER the rename. TestMain sandboxes the daemon package
	// onto a private tmux server, so this cannot touch a real one.
	orphanName := tmux.SanitizedNameForRepo("shared (archived)", repoPath)
	out, err := exec.Command("tmux", "new-session", "-d", "-s", orphanName, "sh").CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+orphanName).Run() })

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    "shared",
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.Error(t, err, "the create must be refused: the unresolvable config is the load-bearing refusal")
	// The unresolvable-config refusal — naming "cannot determine the backend
	// for" and the broken in-repo config — is the one the user must see, not
	// the orphan-tmux refusal validateTitleAvailableLocked would have raised
	// AFTER the rename. Hearing the orphan would send the operator to "kill
	// this tmux session" instead of "fix your repo config", which is the
	// diagnostic mismatch the hoist removes.
	assert.Contains(t, err.Error(), "cannot determine the backend for",
		"the hoisted refusal must win; the orphan-tmux message is what the bug surfaces")
	assert.Contains(t, err.Error(), "failed to parse in-repo config",
		"the actionable cause (the broken in-repo config) must be on the error path")
	assert.NotContains(t, err.Error(), "conflicting tmux session",
		"a record-independent title refusal may not win over the hoisted refusal")
	assert.Nil(t, renamed, "no archived rename may happen for a create that cannot succeed")
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

// TestCreateRPC_UnresolvableConfigRefusesBeforeArchivedRename is the
// end-to-end reproduction via the real control-socket RPC (net/rpc over the
// daemon unix socket), mirroring TestCreateRPC_InvalidHostRefusesBeforeArchivedRename
// for the #5744/#6988 unlisted --host class. It seeds an archived session titled
// "shared" with its branch freed, writes a broken `.agent-factory/config.toml`,
// starts a control server, and issues CreateSession with explicit Title "shared"
// (no ValidateFirst — exactly the TUI shape, where app/session_control.go's
// startSessionThroughDaemon sends the user-typed title with InPlace unset and
// no ValidateFirst, leaving the daemon to own the verdict on an unresolvable
// config).
//
// Hermetic — the unresolvable-config refusal fires inside reserveCreate before
// any network or provision step. Before the fix the create errored (still — the
// canonical refusal fired at NewInstance) but only AFTER reserveCreate had
// renamed the archived session to "shared (archived)": worktree physically
// relocated, manager map re-keyed, instances.json rewritten — permanent state
// corruption with no rollback. After the fix the refusal lands in reserveCreate,
// ahead of the reservation, and the archived session is left untouched.
func TestCreateRPC_UnresolvableConfigRefusesBeforeArchivedRename(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	manager, repoID, repoPath := newStatusTestManager(t)
	// seedArchivedSessionBranchFreed because the only refusal left after the
	// branch is released is the unresolvable-config class this test is about —
	// the exact shape in which the rename would actually fire absent the fix.
	archived, id := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "shared", "shared")
	writeBrokenInRepoConfig(t, repoPath, "this is [[not toml")

	closeServer, err := startControlServer(manager, nil, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	// The exact TUI / task-delivery shape: explicit Title, no ValidateFirst, no
	// --here. Hermetic — the #5801 refusal fires inside reserveCreate ahead of
	// any backend provisioning.
	var resp CreateSessionResponse
	createErr := callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    "shared",
		RepoPath: repoPath,
		Program:  "claude",
	}, &resp)
	require.Error(t, createErr,
		"the create must be refused: the repo's in-repo config cannot be resolved")
	assert.Contains(t, createErr.Error(), "cannot determine the backend for",
		"the refusal must be the canonical #5801 refusal, not a generic create failure")
	assert.Contains(t, createErr.Error(), "failed to parse in-repo config",
		"the refusal must name the broken in-repo config so the user can act on it")
	assert.Empty(t, resp.Instance.ID,
		"no session may be committed for a create refused before the reservation")

	// The archived session must be intact on every surface the rename would
	// have touched — title, manager key, durable record, worktree path, and
	// restorability under the original title.
	assertArchivedUntouched(t, manager, archived, repoID, "shared", id)
}

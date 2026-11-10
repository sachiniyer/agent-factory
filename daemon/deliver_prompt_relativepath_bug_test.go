package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestControlServer_DeliverPrompt_RefusesRelativePath mirrors
// TestControlServer_DeleteProject_RefusesRelativePath (commit 6616c129) and the
// CreateSession RPC's ResolveDaemonHostPath guard (#5743): a relative RepoPath
// must be refused at the RPC boundary, because the daemon resolves what it is
// sent against ITS own cwd — an unrelated checkout for an ad-hoc daemon, / under
// systemd — and DeliverPrompt resolves repo_path via `git -C <path>` AND
// auto-creates a session bound to whatever that resolution lands on. CreateSession,
// DeleteProject, RegisterProject, and RebindProject all enforce this via
// ResolveDaemonHostPath; DeliverPrompt lacked the same guard, so a caller sending
// "." delivered into (and auto-created a session in) the daemon's cwd repo with no
// error the caller could act on (#4821, #5743).
func TestControlServer_DeliverPrompt_RefusesRelativePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	// The daemon's cwd sits inside an unrelated git repo. Before the fix, a
	// relative RepoPath resolved against this cwd and silently delivered into
	// (and auto-created a session in) it.
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)
	cwdRepoCtx, err := config.RepoFromPath(cwdRepo)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	for _, rel := range []string{".", "sub/../.", "relative/path", "   ", "\t"} {
		var resp DeliverPromptResponse
		err := cs.DeliverPrompt(DeliverPromptRequest{
			Title:    "worker",
			RepoPath: rel,
			Program:  "claude",
			Prompt:   "do something",
		}, &resp)
		require.Error(t, err, "relative RepoPath %q must be refused, not resolved against the daemon's cwd", rel)
		assert.Contains(t, err.Error(), "must be absolute",
			"relative RepoPath %q must produce the actionable refusal, not a raw git error or a silent misroute", rel)
		assert.Empty(t, resp.Status, "a refused delivery must not report a status")
	}

	// No session may be auto-created in the cwd repo by a misrouted delivery.
	// Before the fix, "." resolved here and createMissingPromptTarget stored a
	// session under the cwd repo's id; the boundary guard refuses before that.
	raw, err := config.LoadRepoInstances(cwdRepoCtx.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(raw),
		"no session may be auto-created in the daemon's cwd repo by a refused relative-path delivery")
}

// TestControlServer_DeliverPrompt_ExpandsTildeAgainstDaemonHome mirrors the
// sibling RPC boundaries: a leading "~" is expanded against the DAEMON host's
// home, not passed raw to `git -C`. Before the fix, "~" reached git verbatim and
// failed with the non-actionable `failed to get git repo root for ~: ... exit
// status 128; prompt not delivered`; after the fix, ResolveDaemonHostPath expands
// it to a real path so the expanded value — not the literal tilde — reaches git.
func TestControlServer_DeliverPrompt_ExpandsTildeAgainstDaemonHome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	home := testguard.CanonicalTempDir(t)
	// Point HOME at a directory that is NOT a git repo, so the expanded path
	// reaches git and fails there — with the expanded path in the message, not
	// the literal "~" the raw `git -C ~` produced before the fix.
	t.Setenv("HOME", home)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		RepoPath: "~",
		Prompt:   "do something",
	}, &resp)
	require.Error(t, err, "~ expands to a non-repo home dir, so git resolution must fail there")
	assert.NotContains(t, err.Error(), "must be absolute",
		"~ is accepted at the boundary and expanded, not refused as relative")
	assert.Contains(t, err.Error(), home,
		"the expanded HOME path reaches git, not the literal ~")
	assert.Empty(t, resp.Status, "a non-repo home must not report a delivery status")
}

// TestControlServer_DeliverPrompt_UsesTheNormalizedPath mirrors
// TestControlServer_DeleteProject_UsesTheNormalizedPath (#4789 round 7): the
// boundary check trims the path, so the delivery must receive that same trimmed
// value. A whitespace-prefixed absolute path — sent while the daemon's cwd is an
// unrelated checkout — must deliver to the session at the absolute path it
// names, never resolve as relative under the cwd. Checking a trimmed copy while
// passing the raw one would re-open the hole ResolveDaemonHostPath closes.
func TestControlServer_DeliverPrompt_UsesTheNormalizedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	targetRepoPath := setupControlRepo(t)
	targetRepo, err := config.RepoFromPath(targetRepoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	// Register an existing session the padded absolute path must resolve to and
	// deliver into.
	registerStarted(t, manager, targetRepo.ID, targetRepoPath, "worker",
		session.NewFakeBackend(), true, session.Running)

	// Make the daemon's cwd an unrelated checkout so a relative-style resolution
	// would land in the wrong repo.
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)
	cwdRepoCtx, err := config.RepoFromPath(cwdRepo)
	require.NoError(t, err)

	cs := &controlServer{manager: manager}
	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		Title:    "worker",
		RepoPath: "  " + targetRepoPath + "\t",
		Prompt:   "do something",
	}, &resp)
	require.NoError(t, err,
		"the trimmed absolute path must deliver, not resolve as relative under the cwd")
	assert.Equal(t, "sent", resp.Status,
		"the existing target session at the trimmed absolute path received the prompt")

	// The cwd repo must NOT have a session created in it by a misrouted delivery.
	raw, err := config.LoadRepoInstances(cwdRepoCtx.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(raw),
		"the cwd repo must remain untouched by a delivery addressed to an absolute path")
}

// TestControlServer_DeliverPrompt_AbsolutePathAutoCreatesInNamedRepoNotCwd is the
// end-to-end misroute check: with the daemon's cwd inside an UNRELATED git repo,
// a DeliverPrompt addressed by absolute path to a session that does NOT yet
// exist must auto-create that session in the NAMED repo, never in the cwd repo.
// Before the fix a relative "." would create in the cwd repo; this test pins the
// absolute-path happy path across the same chdir so a regression that re-opens
// the misroute fails loudly here.
func TestControlServer_DeliverPrompt_AbsolutePathAutoCreatesInNamedRepoNotCwd(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	targetRepoPath := setupControlRepo(t)
	targetRepo, err := config.RepoFromPath(targetRepoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	// Daemon cwd sits inside an unrelated git repo.
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)
	cwdRepoCtx, err := config.RepoFromPath(cwdRepo)
	require.NoError(t, err)

	cs := &controlServer{manager: manager}
	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		Title:    "worker",
		RepoPath: targetRepoPath,
		Program:  "claude",
		Prompt:   "do something",
	}, &resp)
	require.NoError(t, err, "the absolute path must auto-create and deliver")
	assert.Equal(t, "started", resp.Status, "an absent target auto-creates and reports started")

	// The auto-create lands in the NAMED repo, not the daemon's cwd repo.
	targetRaw, err := config.LoadRepoInstances(targetRepo.ID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(targetRaw, &stored))
	require.Len(t, stored, 1, "the named repo received the auto-created session")
	assert.Equal(t, "worker", stored[0].Title)

	// The cwd repo must NOT receive a session from a delivery addressed to an
	// absolute path.
	cwdRaw, err := config.LoadRepoInstances(cwdRepoCtx.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(cwdRaw),
		"the cwd repo must NOT receive a session from a delivery addressed to an absolute path")
}

// TestControlServer_DeliverPrompt_TaskOriginPreservesTrailingWhitespace pins
// that a task fire (TaskOrigin) replays a stored ProjectPath byte-for-byte:
// ResolveDaemonHostPath trims surrounding whitespace, which would rewrite a
// trailing-space repo path ("/p/repo ") to its trimmed sibling ("/p/repo") — a
// different repo — and break the retained-RepoID check for a bound task on every
// fire (delivery.go:261). A task's RepoID is computed from the raw bytes via
// RepoFromPath (task.repoIDForPath, no trim), so the boundary must not trim a
// path a task's retained identity depends on. Mirrors the CLI contract pinned by
// api.TestTasksUpdate_ProjectPathPreservesWhitespace.
func TestControlServer_DeliverPrompt_TaskOriginPreservesTrailingWhitespace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)

	// Sibling repositories whose names differ only by a trailing space, so the
	// trimmed spelling resolves to a real — but wrong — repo.
	parent := filepath.Join(testguard.CanonicalTempDir(t), "parent")
	trimmedRepoPath := filepath.Join(parent, "repo")
	spacedRepoPath := filepath.Join(parent, "repo ")
	cloneRepoTemplate(t, trimmedRepoPath)
	cloneRepoTemplate(t, spacedRepoPath)
	require.NotEqual(t, trimmedRepoPath, spacedRepoPath,
		"the two spellings must be distinct paths")
	spacedRepo, err := config.RepoFromPath(spacedRepoPath)
	require.NoError(t, err)
	trimmedRepo, err := config.RepoFromPath(trimmedRepoPath)
	require.NoError(t, err)
	require.NotEqual(t, spacedRepo.ID, trimmedRepo.ID,
		"the two spellings must be distinct repositories")

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	// Register an existing session in the spaced (trailing-space) repo that the
	// task fire must deliver into.
	registerStarted(t, manager, spacedRepo.ID, spacedRepoPath, "worker",
		session.NewFakeBackend(), true, session.Running)

	cs := &controlServer{manager: manager}
	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		Title:      "worker",
		RepoPath:   spacedRepoPath,
		Program:    "claude",
		Prompt:     "do something",
		TaskRepoID: spacedRepo.ID,
		TaskOrigin: true,
	}, &resp)
	require.NoError(t, err,
		"a task fire must deliver to the trailing-space repo named by the stored path, not be trimmed to the sibling and fail the retained-RepoID check")
	assert.Equal(t, "sent", resp.Status,
		"the existing target session in the trailing-space repo received the prompt")

	// The trimmed sibling repo must NOT receive a session from a task fire aimed
	// at the trailing-space repo.
	trimmedRaw, err := config.LoadRepoInstances(trimmedRepo.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(trimmedRaw),
		"the trimmed sibling repo must not gain a session from a trailing-space task fire")
}

// TestControlServer_DeliverPrompt_TaskOriginRefusesRelative pins that the
// task-origin path-preservation branch still refuses a relative path: the
// daemon resolves repo_path via `git -C <path>` against its own cwd, so a
// relative task path must be rejected at the boundary even though it is not
// trimmed.
func TestControlServer_DeliverPrompt_TaskOriginRefusesRelative(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp DeliverPromptResponse
	err = cs.DeliverPrompt(DeliverPromptRequest{
		Title:      "worker",
		RepoPath:   ".",
		Program:    "claude",
		Prompt:     "do something",
		TaskRepoID: "anything",
		TaskOrigin: true,
	}, &resp)
	require.Error(t, err, "a relative task RepoPath must be refused, not resolved against the daemon's cwd")
	assert.Contains(t, err.Error(), "must be absolute",
		"a relative task RepoPath must produce the actionable refusal")
	assert.Empty(t, resp.Status, "a refused delivery must not report a status")
}

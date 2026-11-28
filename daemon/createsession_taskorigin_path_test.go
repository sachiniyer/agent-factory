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

// These tests pin the TaskOrigin path-preservation carve-out in
// controlServer.createSession, mirroring the sibling DeliverPrompt boundary
// fixed in #6996 (commit 9dd3c463, daemon/deliver_prompt_relativepath_bug_test.go).
//
// The daemon's task runner (daemon/taskrun.go) fires a task through CreateSession
// with TaskOrigin: true, the task's stored ProjectPath verbatim, and the retained
// TaskRepoID computed from those same raw bytes (task.repoIDForPath, no trim).
// ResolveDaemonHostPath trims surrounding whitespace, so applying it
// unconditionally — as createSession did before this fix — rewrites a
// trailing-space repo path ("/p/repo ") to its trimmed sibling ("/p/repo"), a
// different repo, and the trimmed bytes reach reserveCreateWithWorktreeAdmission's
// retained-RepoID check. A bound task whose stored path names a real git repo is
// then refused on every fire ("task is bound to repo X, but project path ... now
// resolves to repo Y"), and a legacy unbound task is silently created in the
// trimmed sibling. The fix mirrors DeliverPrompt: for TaskOrigin, enforce
// absoluteness on the raw bytes and skip ResolveDaemonHostPath's trim; non-task
// callers keep the full expand-~/trim/refuse-relative guard (#5743/#4789).

// TestControlServer_CreateSession_TaskOriginPreservesTrailingWhitespace pins
// that a task fire (TaskOrigin) replays a stored ProjectPath byte-for-byte through
// CreateSession: ResolveDaemonHostPath trims surrounding whitespace, which would
// rewrite a trailing-space repo path ("/p/repo ") to its trimmed sibling
// ("/p/repo") — a different repo — and break the retained-RepoID check in
// reserveCreateWithWorktreeAdmission on every fire. A task's RepoID is computed
// from the raw bytes via RepoFromPath (task.repoIDForPath, no trim), so the
// boundary must not trim a path a task's retained identity depends on. Mirrors
// TestControlServer_DeliverPrompt_TaskOriginPreservesTrailingWhitespace and the
// CLI contract pinned by api.TestTasksUpdate_ProjectPathPreservesWhitespace.
func TestControlServer_CreateSession_TaskOriginPreservesTrailingWhitespace(t *testing.T) {
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
	cs := &controlServer{manager: manager}

	var resp CreateSessionResponse
	err = cs.CreateSession(CreateSessionRequest{
		Title:      "worker",
		RepoPath:   spacedRepoPath,
		Program:    "claude",
		Prompt:     "do something",
		TaskRepoID: spacedRepo.ID,
		TaskOrigin: true,
	}, &resp)
	require.NoError(t, err,
		"a task fire must create the session in the trailing-space repo named by the stored path, not be trimmed to the sibling and fail the retained-RepoID check")
	assert.Equal(t, "worker", resp.Instance.Title,
		"the created session carries the requested title")

	// The trailing-space repo received the created session.
	spacedRaw, err := config.LoadRepoInstances(spacedRepo.ID)
	require.NoError(t, err)
	var spacedStored []session.InstanceData
	require.NoError(t, json.Unmarshal(spacedRaw, &spacedStored))
	require.Len(t, spacedStored, 1,
		"the trailing-space repo received the created session")
	assert.Equal(t, "worker", spacedStored[0].Title)
	assert.Equal(t, resp.Instance.ID, spacedStored[0].ID,
		"the response identity is the stored session's stable identity")

	// The trimmed sibling repo must NOT receive a session from a task fire aimed
	// at the trailing-space repo.
	trimmedRaw, err := config.LoadRepoInstances(trimmedRepo.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(trimmedRaw),
		"the trimmed sibling repo must not gain a session from a trailing-space task fire")
}

// TestControlServer_CreateSession_TaskOriginTrimmedSiblingRepoIDRefused is the
// inverse proof of the carve-out: a task fire carrying the TRIMMED sibling's
// RepoID against the trailing-space path must now be REFUSED, because the raw
// spaced bytes resolve to the spaced repo (not the trimmed one), so the
// retained-RepoID guard trips. Before the fix, the trim rewrote the path to the
// trimmed sibling, the binding matched, and the session was silently created in
// the WRONG (trimmed) repo with no error — exactly the silent misroute #6996
// closed for DeliverPrompt and this fix closes for CreateSession.
func TestControlServer_CreateSession_TaskOriginTrimmedSiblingRepoIDRefused(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)

	parent := filepath.Join(testguard.CanonicalTempDir(t), "parent")
	trimmedRepoPath := filepath.Join(parent, "repo")
	spacedRepoPath := filepath.Join(parent, "repo ")
	cloneRepoTemplate(t, trimmedRepoPath)
	cloneRepoTemplate(t, spacedRepoPath)
	spacedRepo, err := config.RepoFromPath(spacedRepoPath)
	require.NoError(t, err)
	trimmedRepo, err := config.RepoFromPath(trimmedRepoPath)
	require.NoError(t, err)
	require.NotEqual(t, spacedRepo.ID, trimmedRepo.ID,
		"the two spellings must be distinct repositories")

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp CreateSessionResponse
	err = cs.CreateSession(CreateSessionRequest{
		Title:      "cross-check",
		RepoPath:   spacedRepoPath,
		Program:    "claude",
		Prompt:     "do something",
		TaskRepoID: trimmedRepo.ID, // the TRIMMED sibling's binding
		TaskOrigin: true,
	}, &resp)
	require.Error(t, err,
		"a task fire carrying the trimmed sibling's RepoID must be refused: the raw spaced path resolves to the spaced repo, not the trimmed one")
	assert.Contains(t, err.Error(), "rebind the task to use this project",
		"the retained-RepoID guard must trip on the untrimmed bytes")
	assert.Empty(t, resp.Instance.ID,
		"a refused create must not report a created session")

	// Neither repo may gain a session from a create refused at the
	// retained-RepoID check, which runs before title reservation.
	spacedRaw, err := config.LoadRepoInstances(spacedRepo.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(spacedRaw),
		"the spaced repo must not gain a session from a refused create")
	trimmedRaw, err := config.LoadRepoInstances(trimmedRepo.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(trimmedRaw),
		"the trimmed sibling must not gain a session: the bytes are no longer trimmed to match its binding")
}

// TestControlServer_CreateSession_TaskOriginRefusesRelative pins that the
// task-origin path-preservation branch still refuses a relative path: the daemon
// resolves repo_path via `git -C <path>` against its own cwd, so a relative task
// path must be rejected at the boundary even though it is not trimmed. Mirrors
// TestControlServer_DeliverPrompt_TaskOriginRefusesRelative.
func TestControlServer_CreateSession_TaskOriginRefusesRelative(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp CreateSessionResponse
	err = cs.CreateSession(CreateSessionRequest{
		Title:      "worker",
		RepoPath:   ".",
		Program:    "claude",
		Prompt:     "do something",
		TaskRepoID: "anything",
		TaskOrigin: true,
	}, &resp)
	require.Error(t, err,
		"a relative task RepoPath must be refused, not resolved against the daemon's cwd")
	assert.Contains(t, err.Error(), "must be absolute",
		"a relative task RepoPath must produce the actionable refusal")
	assert.Empty(t, resp.Instance.ID,
		"a refused create must not report a created session")
}

// TestControlServer_CreateSession_NonTaskOriginTrimsPaddedPath pins that the
// carve-out did not relax the boundary for ordinary (non-task) callers: a
// whitespace-padded absolute path is still trimmed by ResolveDaemonHostPath and
// the create lands in the named repo, never resolved relative under the daemon's
// cwd. This is the #5743/#4789 protection the else branch preserves.
func TestControlServer_CreateSession_NonTaskOriginTrimsPaddedPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	targetRepoPath := setupControlRepo(t)
	targetRepo, err := config.RepoFromPath(targetRepoPath)
	require.NoError(t, err)

	// Make the daemon's cwd an unrelated checkout so a relative-style resolution
	// would land in the wrong repo.
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)
	cwdRepoCtx, err := config.RepoFromPath(cwdRepo)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	var resp CreateSessionResponse
	err = cs.CreateSession(CreateSessionRequest{
		Title:    "worker",
		RepoPath: "  " + targetRepoPath + "\t",
		Program:  "claude",
		Prompt:   "do something",
	}, &resp)
	require.NoError(t, err,
		"a non-task-origin padded absolute path must be trimmed and create in the named repo, not resolve as relative under the cwd")
	assert.Equal(t, "worker", resp.Instance.Title)

	raw, err := config.LoadRepoInstances(targetRepo.ID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1,
		"the named repo received the created session from the trimmed path")
	assert.Equal(t, "worker", stored[0].Title)

	// The cwd repo must NOT receive a session from a create addressed to an
	// absolute path.
	cwdRaw, err := config.LoadRepoInstances(cwdRepoCtx.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(cwdRaw),
		"the cwd repo must remain untouched by a create addressed to an absolute path")
}

// TestControlServer_CreateSession_NonTaskOriginRefusesRelative pins that the
// carve-out did not relax the relative-path refusal for ordinary callers: a
// relative RepoPath is still rejected at the boundary with the actionable
// "must be absolute" error, never resolved against the daemon's cwd.
func TestControlServer_CreateSession_NonTaskOriginRefusesRelative(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	cwdRepo := filepath.Join(testguard.CanonicalTempDir(t), "cwd-repo")
	cloneRepoTemplate(t, cwdRepo)
	t.Chdir(cwdRepo)
	cwdRepoCtx, err := config.RepoFromPath(cwdRepo)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	cs := &controlServer{manager: manager}

	for _, rel := range []string{".", "relative/path"} {
		var resp CreateSessionResponse
		err := cs.CreateSession(CreateSessionRequest{
			Title:    "worker",
			RepoPath: rel,
			Program:  "claude",
			Prompt:   "do something",
		}, &resp)
		require.Error(t, err,
			"relative RepoPath %q must be refused, not resolved against the daemon's cwd", rel)
		assert.Contains(t, err.Error(), "must be absolute",
			"relative RepoPath %q must produce the actionable refusal, not a raw git error or a silent misroute", rel)
		assert.Empty(t, resp.Instance.ID,
			"a refused create must not report a created session")
	}

	// No session may be created in the cwd repo by a misrouted create.
	raw, err := config.LoadRepoInstances(cwdRepoCtx.ID)
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(raw),
		"no session may be created in the daemon's cwd repo by a refused relative-path create")
}

package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// #7754 property 2, the resolution arm: a destructive request registers the
// caller under the targets of the session the request actually resolves — its
// tmux sessions and its worktree — so the exemption belongs to the teardown
// the requester is waiting on and to no other.
func TestTeardownTargetsForSession_ResolvesTheRequestedSession(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, worktree := registerArchivable(t, manager, repoID, repoPath, "target-me")
	ts := sessiontmux.NewTmuxSessionFromSanitizedName("af_target_me", "claude")
	inst.SetTmuxSession(ts)

	targets := manager.teardownTargetsForSession("", "target-me", repoID)

	assert.Contains(t, targets, teardownreq.TmuxTarget("af_target_me"),
		"the requester must be excused from the pane reap of the session it asked to stop")
	assert.Contains(t, targets, teardownreq.WorktreeTarget(worktree),
		"and from the writer reap of that same session's worktree")

	// A DIFFERENT session's teardown gets nothing from this registration —
	// the incident the property guards: an af client blocked on archiving B
	// while living in A's pane must still be reapable by A.
	other := []teardownreq.Target{
		teardownreq.TmuxTarget("af_someone_else"),
		teardownreq.WorktreeTarget("/other/worktree"),
	}
	requester := proctree.Process{PID: 424243, StartID: 8}
	untrack := sessiontmux.TrackTeardownRequester(requester, targets...)
	defer untrack()
	assert.True(t, teardownreq.Is(requester, teardownreq.TmuxTarget("af_target_me")))
	assert.False(t, teardownreq.Is(requester, other[0]))
	assert.False(t, teardownreq.Is(requester, other[1]))
}

// A request that names no live instance — a ghost record, a bad title, a
// session already torn down — resolves to no targets. The registration then
// holds nothing, exempting nobody: the refused request leaves its caller
// exactly as reapable as before #5182's exemption existed.
func TestTeardownTargetsForSession_UnresolvableExemptsNothing(t *testing.T) {
	manager, repoID, _ := newStatusTestManager(t)

	assert.Empty(t, manager.teardownTargetsForSession("no-such-id", "no-such-title", repoID),
		"an unresolvable request must not guess a target")

	requester := proctree.Process{PID: 424244, StartID: 9}
	untrack := sessiontmux.TrackTeardownRequester(requester, manager.teardownTargetsForSession("", "no-such-title", repoID)...)
	defer untrack()
	assert.False(t, teardownreq.Is(requester, teardownreq.TmuxTarget("af_anything")),
		"a refused request registers an exemption for nobody")
}

// The resolver is id-first — the same resolution the operation itself uses —
// so a request that carries an id resolves THAT instance's targets even when
// the title field would point elsewhere (or nowhere).
func TestTeardownTargetsForSession_ResolvesByIDOverTitle(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "id-wins")
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName("af_id_wins", "claude"))

	targets := manager.teardownTargetsForSession(inst.ID, "wrong-title", repoID)
	assert.Contains(t, targets, teardownreq.TmuxTarget("af_id_wins"),
		"the registration must name the instance the teardown will actually reach")
}

// #7754 R1, through the real archive handler: the request a self-archive is
// waiting on registers the whole set the operation's reapers can act on —
// the tmux names it closes, the relocation SOURCE the forward move and the
// rollback-home sweep, and the archive DESTINATION the rollback sweeps
// writers at when the caller's cwd followed the directory there. On the
// pre-fix head the destination is missing and the blocked caller reads as a
// survivor at the exact path the undo reaps (#5182 again, one step later).
func TestArchiveSession_RegistersEveryReapPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "self-archive")
	inst.SetTmuxSession(sessiontmux.NewTmuxSessionFromSanitizedName("af_self_archive", "claude"))
	requester := proctree.Process{PID: 424250, StartID: 21}
	server := &controlServer{manager: manager, requester: &requester}

	var sawTmux, sawSource, sawDest, sawOther bool
	prev := archiveTeardown
	archiveTeardown = func(_ *session.Instance, dest string, claim sessiongit.RelocationClaim, _ func() error, _ bool) (error, error) {
		sawTmux = teardownreq.Is(requester, teardownreq.TmuxTarget("af_self_archive"))
		sawSource = teardownreq.Is(requester, teardownreq.WorktreeTarget(claim.Path))
		sawDest = teardownreq.Is(requester, teardownreq.WorktreeTarget(dest))
		sawOther = teardownreq.Is(requester, teardownreq.TmuxTarget("af_someone_else"))
		return nil, teardownRefusal
	}
	t.Cleanup(func() { archiveTeardown = prev })

	var resp ArchiveSessionResponse
	err := server.archiveSession(context.Background(), &ArchiveSessionRequest{Title: "self-archive", RepoID: repoID}, &resp)

	require.ErrorIs(t, err, teardownRefusal)
	assert.True(t, sawTmux, "the caller must be excused from its own pane reap")
	assert.True(t, sawSource, "the claim's selected source must be registered — the move and its rollback both reap there")
	assert.True(t, sawDest,
		"the archive destination must be registered — the rollback's writer sweep runs at the path the caller's cwd followed")
	assert.False(t, sawOther, "another session's teardown still gets nothing")
	assert.False(t, teardownreq.Is(requester, teardownreq.TmuxTarget("af_self_archive")),
		"the exemption ends with the reply, as before")
}

// #7754 R4: a relocation claim may select the recovery's ALTERNATE path as the
// source its reaper sweeps — the worktree path read at snapshot time is not
// the only candidate. The registration must carry both.
func TestTeardownTargetsForSession_IncludesRecoveryAlternatePath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, worktree := registerArchivable(t, manager, repoID, repoPath, "alt-path")
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	alternate := filepath.Join(t.TempDir(), "archived-alt")
	require.NoError(t, gw.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:             sessiongit.RelocationRecoveryCleanupReady,
		AlternatePath:     alternate,
		IdentityKnown:     true,
		Device:            1,
		Inode:             2,
		CleanupGeneration: "gen-1",
	}))

	targets := manager.teardownTargetsForSession("", "alt-path", repoID)

	assert.Contains(t, targets, teardownreq.WorktreeTarget(worktree),
		"the primary worktree path stays a target")
	assert.Contains(t, targets, teardownreq.WorktreeTarget(alternate),
		"the recovery's alternate path must be a target — a claim can select it as the reap source")
}

// #7754 R2, through the real kill handler: a disk-only ghost resolved by
// title proceeds to the REAL ghost cleanup — which kills every tmux name the
// record enumerates and sweeps the record's worktree. The registration must
// name exactly that record's targets, not stand empty because there is no
// live instance to snapshot.
func TestKillSession_GhostRegistersItsRecordsTargets(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	worktree := filepath.Join(t.TempDir(), "wt-ghost")
	seeded, err := json.Marshal([]session.InstanceData{{
		Title:    "ghost",
		Path:     repoPath,
		Status:   session.Ready,
		TmuxName: "af_ghost",
		Tabs: []session.TabData{
			{ID: "agent-1", Kind: session.TabKindAgent, Name: "agent", TmuxName: "af_ghost"},
			{ID: "shell-1", Kind: session.TabKindShell, Name: "shell", TmuxName: "af_ghost__shell"},
		},
		PendingTabCleanup: []session.TabCleanupData{{TabID: "gone-1", TmuxName: "af_ghost__gone"}},
		Worktree: session.GitWorktreeData{
			RepoPath: repoPath, WorktreePath: worktree, SessionName: "af_ghost", BranchName: "af/ghost",
		},
	}})
	require.NoError(t, err)
	require.NoError(t, config.LoadState().SaveInstances(repoID, seeded))

	requester := proctree.Process{PID: 424251, StartID: 22}
	server := &controlServer{manager: manager, requester: &requester}

	exempted := map[string]bool{}
	prevKill := ghostKillTmuxByName
	ghostKillTmuxByName = func(name string) (sessiontmux.PaneState, bool, error) {
		exempted["tmux:"+name] = teardownreq.Is(requester, teardownreq.TmuxTarget(name))
		return sessiontmux.PaneStateKnown, false, nil
	}
	t.Cleanup(func() { ghostKillTmuxByName = prevKill })
	prevWT := ghostCleanupWorktree
	ghostCleanupWorktree = func(data *session.InstanceData, _ string, _ func(*session.InstanceData) error) (sessiongit.CleanupState, error, <-chan error) {
		exempted["wt:"+data.Worktree.WorktreePath] = teardownreq.Is(requester, teardownreq.WorktreeTarget(data.Worktree.WorktreePath))
		return sessiongit.CleanupSettled, nil, nil
	}
	t.Cleanup(func() { ghostCleanupWorktree = prevWT })

	var resp KillSessionResponse
	require.NoError(t, server.killSession(context.Background(), &KillSessionRequest{Title: "ghost", RepoID: repoID}, &resp))

	for _, name := range []string{"af_ghost", "af_ghost__shell", "af_ghost__gone"} {
		assert.True(t, exempted["tmux:"+name],
			"the caller must be excused from the ghost's %q kill — the same record the cleanup kills", name)
	}
	assert.True(t, exempted["wt:"+worktree],
		"the caller must be excused from the record's writer sweep")
}

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
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

package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// This file covers the SURFACE half of the usage-limit retry (#1934): that the
// verb is reachable by a client other than the TUI, and that a client which holds
// stable ids can use one. The behavior of the resume itself (respawn vs stall,
// which prompt is re-delivered, teardown interlocks) is limit_resume_test.go's.

// TestResumeFromLimit_IsInThePublicCatalog is the #1934 acceptance criterion at
// the wire level.
//
// A client can only call what HTTPRoutes() advertises: the web builds its request
// list from the catalog, and `af api` prints it. So while this verb sat in
// internalHTTPRoutes, the web could render a session as limit-blocked — its own
// glyph, label and "[limit] resets …" title prefix — and offer no way out of it.
// The STATE was deliberately surfaced on every surface; the EXIT existed on one.
//
// daemon/httproutes.go called it "a genuine client-facing session verb" whose
// promotion was "a one-line follow-up". This test is what keeps that follow-up
// from being un-done: parking it back in the internal table is a silent
// regression, because nothing else fails when a verb is merely unreachable.
func TestResumeFromLimit_IsInThePublicCatalog(t *testing.T) {
	const path = "/v1/ResumeFromLimit"

	var inPublic bool
	for _, rt := range HTTPRoutes() {
		if rt.Path == path {
			inPublic = true
			assert.Contains(t, rt.RequestFields, "id",
				"the catalog must advertise the id field, or a client cannot know it may address the session by stable id")
		}
	}
	assert.True(t, inPublic, "%s must be in the PUBLIC catalog: a client can only call what HTTPRoutes() advertises, "+
		"and a limit-blocked session that no client can resume is #1934", path)

	for _, rt := range internalHTTPRoutes {
		assert.NotEqual(t, path, rt.Path,
			"%s was promoted out of internalHTTPRoutes in #1934; moving it back makes the web's Retry button "+
				"unreachable again, and nothing else would fail", path)
	}
}

// TestResumeFromLimit_ResolvesByStableID pins the id-first addressing the web
// needs, and the reason the request grew an ID field at all.
//
// Two repos, one shared title. Resolving by title alone is ambiguous, and this
// verb RE-DELIVERS A PROMPT into a pane — so a misroute types someone's
// instruction into an unrelated repo's agent. That is the unstable-identity class
// (#1904) this repo has paid for repeatedly, which is why every other web-facing
// mutation (kill, archive) already keys by id.
//
// The assertion is not merely "it resolved": it is that the resume acted on the
// session with the requested ID and left the other one alone.
func TestResumeFromLimit_ResolvesByStableID(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	backendA := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	instA := registerStarted(t, manager, repoID, repoPath, "collide", backendA, true, session.Running)
	instA.Prompt = ""
	instA.SetLimitReached(time.Now())
	require.NotEmpty(t, instA.ID, "precondition: the target must carry a stable id")

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{ID: instA.ID}); err != nil {
		t.Fatalf("resumeFromLimit by id returned %v, want nil — an id-only request must resolve without a title", err)
	}

	_, _, prompts := backendA.snapshot()
	assert.Len(t, prompts, 1, "the session named by the id must have been resumed")
	assert.False(t, instA.LimitReached(), "a successful resume clears the limit state")
}

// TestResumeFromLimit_UnknownIDIsRefused is the other half, and the one that
// matters for safety.
//
// A stale id — the session was killed between the browser rendering the button
// and the user clicking it — must FAIL, not fall back to the title. Falling back
// is what turns a stale click into a prompt delivered to whatever session happens
// to share the name, which is precisely the misroute the id exists to prevent.
func TestResumeFromLimit_UnknownIDIsRefused(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "parked", backend, true, session.Running)
	inst.Prompt = ""
	inst.SetLimitReached(time.Now())

	// The title is CORRECT and would resolve on its own. The id is not. A
	// fallback-on-miss implementation would resume `parked` here and pass.
	err := manager.resumeFromLimit(ResumeFromLimitRequest{
		ID:     "id-that-no-longer-exists",
		Title:  "parked",
		RepoID: repoID,
	})

	require.Error(t, err, "an unresolvable id must be refused, never silently resolved by title instead")
	_, _, prompts := backend.snapshot()
	assert.Empty(t, prompts, "no prompt may be delivered to a session the request did not name")
	assert.True(t, inst.LimitReached(), "the untargeted session stays parked")
}

// TestResumeFromLimit_TitleStillResolves keeps the TUI and CLI path working: they
// are repo-scoped, send no id, and must go on resolving by {Title, RepoID}. The
// id is an ADDITION for id-holding clients, not a new requirement — the same
// contract KillSessionRequest.ID carries.
func TestResumeFromLimit_TitleStillResolves(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	backend := &limitResumeBackend{FakeBackend: session.NewFakeBackend(), alive: true}
	inst := registerStarted(t, manager, repoID, repoPath, "by-title", backend, true, session.Running)
	inst.Prompt = ""
	inst.SetLimitReached(time.Now())

	if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "by-title", RepoID: repoID}); err != nil {
		t.Fatalf("resumeFromLimit by title returned %v, want nil — the TUI/CLI path must be unchanged", err)
	}

	_, _, prompts := backend.snapshot()
	assert.Len(t, prompts, 1)
	assert.False(t, inst.LimitReached())
}

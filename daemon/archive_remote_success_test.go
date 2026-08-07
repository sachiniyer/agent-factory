package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// A SUCCESSFUL remote archive had no test at all (#3037), and the success path is
// where the irreversible work happens: it pushes the branch to origin, records
// that branch on the instance, and reaps the sandbox.
//
// The branch recording is the load-bearing part, and it is why this gap mattered
// more than a normal one. Daemon-side Branch has exactly one writer for a sandbox
// session — the Archive() return — so if it is ever dropped, `RestoreBranch` is
// empty on the way back and BOTH sandbox runtimes SKIP the restore fetch, which
// brings the session up on the repository's DEFAULT branch with its work stranded
// under a ref nothing points at. That is the #2923/#2925/#2959 data loss, and an
// archive reports success either way, so the regression would be silent.
//
// The ordering matters as much as the value: the branch is recorded the instant
// the push makes it durable, BEFORE the teardown, so a failed reap cannot lose
// the only handle on the user's work.
func TestArchiveRemoteSession_RecordsThePushedBranchAndReapsOnce(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "remote-worker", srv.url, session.Running)

	// The PHYSICAL reap, installed through the same field a sandbox runtime
	// populates. Counting the /v1/agent/kill request instead would pass for a
	// regression that sent the message and left the container running (#3042).
	//
	// The witness samples the push count AT THE MOMENT OF THE REAP, which is what
	// makes "push before release" an assertion about ordering rather than about two
	// totals. The fixture refusing /v1/agent/kill before /v1/agent/archive already
	// orders the two REST calls, but the reap is a separate act from the call that
	// triggers it: a regression that reaped inside the kill handler's error path, or
	// from any site that runs before the push, leaves both REST counters at one and
	// every branch assertion passing while the workspace being made durable is
	// already destroyed.
	reap.observe(srv.archiveCalls.Load)

	require.Empty(t, inst.GetBranch(),
		"precondition: a never-archived sandbox session has no branch recorded")

	_, ch := manager.events.subscribe()

	_, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "remote-worker", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, "af/pushed-branch", inst.GetBranch(),
		"the branch the push returned is the ONLY handle on this session's work; an empty one makes "+
			"the restore skip its fetch and come back on the repository's default branch")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness())

	if rec := recordFor(t, repoID, "remote-worker"); assert.NotNil(t, rec) {
		assert.Equal(t, "af/pushed-branch", rec.Branch,
			"and it must be DURABLE — a restore after a daemon restart reads the record, not memory")
		assert.Equal(t, session.LiveArchived, rec.Liveness)
	}

	assert.Equal(t, 1, reap.count(),
		"the sandbox runtime must actually be RELEASED exactly once — a kill that reports success "+
			"while the container keeps running leaves a VM billing with no session record pointing "+
			"at it, so nothing will ever reap it")
	assert.Equal(t, int32(1), reap.witnessedAtFirstReap(),
		"and the release must happen with the push ALREADY DONE: the reap read 0 pushes from its own "+
			"vantage point, so the sandbox holding the only copy of this work was destroyed before the "+
			"work was made durable (#2923/#2925/#2959)")
	assert.Equal(t, int32(1), srv.killCalls.Load(),
		"the request that triggers the reap is sent once too — a second one means two paths each "+
			"believe they own this runtime's lifetime")
	assert.Equal(t, int32(1), srv.archiveCalls.Load(),
		"the push runs exactly once: twice would mean the sandbox was archived again after its "+
			"branch was already durable")

	published := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	assert.Equal(t, "remote-worker", published.Title)
	assert.Equal(t, "af/pushed-branch", published.Branch,
		"every other client learns the archived branch from this event; a projection without it "+
			"cannot offer a correct restore")
	assert.Equal(t, archived.ID, published.ID)
}

package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The half-failed remote archive: the push LANDED and the reap did not.
//
// This is the arm a message assertion cannot reach at all, which is why #3042
// blocked it rather than merely weakening it. Everything that distinguishes this
// outcome from a clean archive is about the physical runtime — whether it was
// released, whether af still holds the handle to try again — and the REST layer
// reports the same /v1/agent/kill success in both cases. With no teardown
// installed the reap could not fail, so this path had no daemon-level test.
//
// It is also the arm where a mistake is worst. The push is the point of no return:
// from here the pushed branch is the ONLY handle on the user's work, and the
// sandbox that knows the name is the thing whose fate is uncertain. So the
// invariants are all about not losing that handle while the reap is unresolved —
// record the branch, stay recovery-eligible rather than Archived, and keep the
// wiring so the reap is retryable instead of leaking a container af has forgotten.
func TestArchiveRemoteSession_ReapFailsUnknown_KeepsTheBranchAndTheRetryableHandle(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := newSandboxProbeServer(t, "af/pushed-branch")
	inst, _, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "reap-unknown", srv.url, session.Running)

	// UNKNOWN state, not a plain failure, and the distinction is the whole taxonomy:
	// "the reap errored" can still mean the container is gone (a dead endpoint after a
	// successful docker rm), whereas this says af cannot tell. Only the second may
	// retain anything, so a fixture that failed the reap generically would exercise
	// the wrong branch of TeardownStateUnknown.
	reap.fail(fmt.Errorf("docker rm timed out: %w", session.ErrWorkspaceStateUnknown))

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "reap-unknown", RepoID: repoID})
	require.Error(t, err, "an archive whose sandbox reap state is unknown must not report success")

	// ATTEMPTED, exactly once. Zero would mean the archive never tried to release the
	// runtime and merely posted the REST kill — the #3042 regression itself. Two would
	// mean a retry ran inside the failure path, which is where an unknown reap turns
	// into a second reap against a replacement runtime.
	assert.Equal(t, 1, reap.count(),
		"the physical reap must be attempted exactly once per archive attempt")

	// The branch survives the failed reap, in memory AND on disk. ArchiveSandbox
	// records it the instant the push makes it durable, before the teardown, precisely
	// so this outcome cannot lose it: a sandbox session's daemon-side branch has only
	// one writer, so an empty one here makes the Lost-restore loop re-provision with an
	// empty RestoreBranch — which both sandbox runtimes read as "skip the restore
	// fetch", bringing the session up on the repository's default branch with this
	// work stranded (#2923/#2925/#2959).
	assert.Equal(t, "af/pushed-branch", inst.GetBranch())
	rec := recordFor(t, repoID, "reap-unknown")
	require.NotNil(t, rec, "the record must survive an archive that could not finish")
	assert.Equal(t, "af/pushed-branch", rec.Branch,
		"and DURABLY: the retry reads the record, not memory, so a daemon restart here must still "+
			"know which branch holds the work")

	// Lost and recovery-eligible, NOT Archived. Archived is the committed, inert state
	// — it means the sandbox is gone and the branch is on origin. Claiming it while a
	// container may still be running is the lie that leaves a VM billing with nothing
	// pointing at it.
	assert.Equal(t, session.LiveLost, rec.Liveness,
		"a half-failed archive rolls back to Lost (AbortArchiveToLost) so the restore loop keeps "+
			"working on it; committing Archived here would claim the sandbox is gone while a container "+
			"may still be running")

	// The reap is still OWED, and durably so. This marker is what makes a restart
	// retry the cleanup before provisioning a replacement, instead of stacking a second
	// sandbox on top of one it forgot about.
	assert.True(t, rec.RuntimeCleanupStateUnknown,
		"an indeterminate reap must be recorded as still owed; without it a restart provisions a "+
			"replacement alongside a container nothing will ever come back for")

	// And the handle is RETAINED, asserted the only way that means anything: run the
	// reap again and watch a second release actually happen. An unknown outcome keeps
	// the wiring installed for exactly this retry — the success path is the one that
	// resets it — so a regression that cleared it here would leave af holding no way
	// to reach the runtime it just failed to destroy.
	reap.fail(nil)
	_, retryErr := inst.ArchiveSandbox()
	require.NoError(t, retryErr,
		"the retained wiring must still be able to push and reap; a refusal here means the handle was "+
			"dropped and the container is unreachable")
	assert.Equal(t, 2, reap.count(), "the retry must reach the PHYSICAL reap, not just re-send the REST kill")
}

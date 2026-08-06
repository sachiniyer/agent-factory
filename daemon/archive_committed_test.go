package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// failArchivePersist forces the durable archive write to fail, which is the one
// lever that separates "the archive committed" from "the daemon recorded it".
func failArchivePersist(t *testing.T, failure error) func() {
	t.Helper()
	prev := archivePersist
	archivePersist = func(*Manager, string, *session.Instance) error { return failure }
	restore := func() { archivePersist = prev }
	t.Cleanup(restore)
	return restore
}

// A remote archive whose durable write fails has still COMMITTED (#3029).
//
// The branch is on origin and the sandbox is reaped — both irreversible — so the
// two things a caller and every other client need are exactly the two this path
// omitted:
//
//   - the archived EVENT, because it is how a TUI or web rail learns what
//     happened. Without it they keep rendering a session that is archived in fact,
//     and nothing reconciles the divergence;
//   - the COMMITTED marker, because "failed" and "failed after committing" demand
//     opposite handling. A caller that retries this is not recovering, it is
//     re-running a partially applied change against a sandbox that no longer
//     exists.
//
// Only a failure with nothing committed is safe to retry blindly, which is why
// this path must be distinguishable from that one rather than collapsed into it.
func TestArchiveRemoteSession_PersistFailureIsReportedAsCommitted(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-worker",
		newSandboxProbeServer(t, "af/pushed-branch").url, session.Running)
	_ = backend

	_, events := manager.events.subscribe()

	diskFull := errors.New("no space left on device")
	restore := failArchivePersist(t, diskFull)
	defer restore()

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "remote-worker", RepoID: repoID})

	require.Error(t, err, "a durable write that did not land is not a successful archive")
	require.ErrorIs(t, err, diskFull)

	var committed interface{ MutationCommitted() bool }
	require.ErrorAs(t, err, &committed,
		"the archive committed — branch pushed, sandbox reaped — so the caller must be told not to "+
			"retry it; a plain error is indistinguishable from one where nothing happened")
	assert.True(t, committed.MutationCommitted())

	// drainNextSessionEvent fails the test if nothing arrives, which is the whole
	// assertion: every other client learns the session was archived only from this
	// event, and without it the rail keeps showing a session that is archived on disk.
	archived := drainNextSessionEvent(t, events, agentproto.EventSessionArchived)
	assert.Equal(t, "remote-worker", archived.Title)

	require.Equal(t, session.LiveArchived, inst.GetLiveness(),
		"premise: the archive really did commit in memory")
}

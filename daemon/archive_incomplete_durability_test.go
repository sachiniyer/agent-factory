package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// incompleteArchiveReport is the shape that routes an archive to
// keepIncompleteArchiveCommitted: a retained source tree holding the only
// complete copy of a file the published archive had to skip. Rolling that back
// would drop the retained bytes, so the archive is kept instead.
func incompleteArchiveReport() git.ArchiveReport {
	return git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{
		Path:          "/worktrees/.af-source-0123456789abcdef0123456789abcdef",
		IdentityKnown: true,
		Device:        1,
		Inode:         2,
		FileType:      0o040000,
		Skipped: []git.ArchiveSkippedEntry{{
			Path:   "private/credential",
			Reason: git.ArchiveSkipPermissionDenied,
		}},
	}}}
}

func markArchiveIncomplete(t *testing.T, inst *session.Instance) {
	t.Helper()
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	gw.RestoreArchiveReport(incompleteArchiveReport())
}

// #3448. keepIncompleteArchiveCommitted returned the committed marker even when
// the durable write that backs the committed claim failed. DeleteProject reads a
// committed error as success-with-warning: it records the message and goes on to
// DEREGISTER the project. With the Archived row never on disk, that deregisters
// on top of a stale PRE-ARCHIVE live row — a restart reloads it, cannot rebuild
// the worktree at the vacated path, and skips the instance, leaving the bytes
// orphaned under the archive with no project and no session pointing at them.
//
// This is the rule the sibling keepUnrollableArchiveCommitted already enforces:
// the committed claim must BE durable before it is made.
func TestKeepIncompleteArchiveCommitted_UndurableWriteIsNotCommitted(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	markArchiveIncomplete(t, inst)

	prev := archivePersist
	persistCalls := 0
	archivePersist = func(*Manager, string, *session.Instance) error {
		persistCalls++
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.keepIncompleteArchiveCommitted(
		repoID, "/archived/worker", inst, nil,
		errors.New("final VS Code editor teardown was not confirmed"),
	)

	require.Error(t, err)
	require.Equal(t, 1, persistCalls, "the durable write must actually be attempted before the claim is judged")
	assert.False(t, isMutationCommitted(err),
		"an undurable archive must not claim committed: DeleteProject would deregister over a stale live row")
	assert.Contains(t, err.Error(), "/archived/worker",
		"the plain shape must still name where the kept bytes are, for manual recovery")
	assert.Contains(t, err.Error(), "final VS Code editor teardown was not confirmed",
		"the original cause must survive into the plain error")
	assert.Empty(t, archivedPath)
	assert.Empty(t, archived.ID)
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionArchived {
				t.Fatal("no archived event may be published for an archive whose durable claim never landed")
			}
			continue
		default:
		}
		break
	}
}

// The other direction, so the fix cannot decay into "an incomplete archive is
// never committed": when the durable write lands, the incomplete archive is
// still kept and still reported committed, with its location, its projection,
// its skipped-file report, and its session.archived event (#3235).
func TestKeepIncompleteArchiveCommitted_DurableWriteStillClaimsCommitted(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")
	markArchiveIncomplete(t, inst)

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.keepIncompleteArchiveCommitted(
		repoID, "/archived/worker", inst, nil,
		errors.New("final VS Code editor teardown was not confirmed"),
	)

	require.Error(t, err)
	assert.True(t, isMutationCommitted(err),
		"a durable incomplete archive IS committed: callers must not retry the landed move")
	assert.Equal(t, "/archived/worker", archivedPath, "the kept location must reach the caller")
	assert.Equal(t, inst.ID, archived.ID)
	assert.Contains(t, err.Error(), "private/credential",
		"the skipped-file report must still reach every transport")

	archEv := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	assert.Equal(t, inst.ID, archEv.ID)
}

// The adjacent call site, end to end. ArchiveSession reaches
// keepIncompleteArchiveCommitted a second way: its own durable write failed and
// the report is non-empty. That road used to hand the helper needsDurableWrite
// =false, which retried the write through persistInstance, DISCARDED the error,
// and claimed committed regardless — the identical undurable claim, reached by
// the road that is BY CONSTRUCTION taken because the disk just failed. Both
// callers now go through the one gate.
func TestArchiveSession_IncompleteAndUndurable_StaysPlainFailure(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, _ = registerArchivable(t, manager, repoID, repoPath, "worker")

	// The copier's real report depends on a permission-denied file the test user
	// may be able to read, so stamp the incomplete shape on after the real
	// teardown has moved the worktree.
	prevTeardown := archiveTeardown
	archiveTeardown = func(inst *session.Instance, dest string, claim git.RelocationClaim, before func() error) (error, error) {
		hookErr, archiveErr := prevTeardown(inst, dest, claim, before)
		if archiveErr == nil {
			markArchiveIncomplete(t, inst)
		}
		return hookErr, archiveErr
	}
	t.Cleanup(func() { archiveTeardown = prevTeardown })

	prev := archivePersist
	persistCalls := 0
	archivePersist = func(*Manager, string, *session.Instance) error {
		persistCalls++
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { archivePersist = prev })

	_, ch := manager.events.subscribe()
	archivedPath, archived, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})

	require.Error(t, err)
	require.Equal(t, 2, persistCalls,
		"precondition: the commit write failed and the keep-incomplete helper retried it")
	assert.False(t, isMutationCommitted(err),
		"an undurable incomplete archive must not claim committed: DeleteProject would deregister over a stale live row")
	assert.Empty(t, archivedPath)
	assert.Empty(t, archived.ID)
	for {
		select {
		case ev := <-ch:
			if ev.Type == agentproto.EventSessionArchived {
				t.Fatal("no archived event may be published for an archive whose durable claim never landed")
			}
			continue
		default:
		}
		break
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestArchiveSession_UnprovenResidueBindingSaysTransient pins #7900 on the
// dest-absent arm of archiveSessionSourceAbsent: when the Unknown verdict on
// a residue candidate came from a component af could not read — an EACCES
// opening the candidate itself — the refusal says the binding failed on that
// component and that a retry may clear once it is readable, not only "it may
// belong to another session… move it out". The verdict stays Unknown and the
// refusal stays fail-closed; this only changes the wording.
func TestArchiveSession_UnprovenResidueBindingSaysTransient(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability is meaningless as root")
	}
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "unproven-residue")
	dest, err := archivedWorktreePath(repoID, "unproven-residue")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	// The recorded worktree is absent, and a residue-shaped directory whose
	// own mode bits refuse af's open sits beside it: BindAFRelocationResidue
	// cannot even read the candidate, so its error is an unproven EACCES —
	// the transient class #7900 gives its own wording.
	require.NoError(t, os.RemoveAll(wtPath))
	unreadable := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("e", 32))
	require.NoError(t, os.Mkdir(unreadable, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unreadable, "stale.txt"), []byte("orphan"), 0o644))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "unproven-residue", RepoID: repoID})
	require.Error(t, err, "an unreadable residue must refuse fail-closed")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the verdict stays Unknown — unproven never becomes ours or foreign")
	assert.Contains(t, err.Error(),
		"could not be read; the binding failed on "+unreadable+" — retry once "+unreadable+" is readable; inspect "+unreadable,
		"the exact adjacency: the transient clause follows the could-not-read text and the residue list lands on the inspect verb (#7900 R1)")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal still says the residue is unverified, not this session's work")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing over unproven residue must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released")
	assert.DirExists(t, unreadable,
		"the unreadable residue must be left untouched for the operator")
}

// TestArchiveSession_DestPresentUnprovenQuarantineSaysTransient pins #7900 on
// the dest-present arm: a `.af-source-*`-shaped residue beside the vacated
// source that af cannot even open lands in unverifiedQuarantine, and when the
// failure is the unproven class the adoption refusal names the component and
// says a retry may clear once it is readable — on BOTH binds (dest and
// source anchors), either of which can produce the transient error.
func TestArchiveSession_DestPresentUnprovenQuarantineSaysTransient(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadability is meaningless as root")
	}
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-unproven")
	dest, err := archivedWorktreePath(repoID, "dest-unproven")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	// A source-quarantine-shaped residue af cannot open: both the dest and
	// the source anchor binds fail on the same EACCES, so the candidate is
	// unverified AND unproven.
	unreadable := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("e", 32))
	require.NoError(t, os.Mkdir(unreadable, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unreadable, "stale.txt"), []byte("orphan"), 0o644))
	require.NoError(t, os.Chmod(unreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-unproven", RepoID: repoID})
	require.Error(t, err,
		"an unreadable quarantine-shaped residue must refuse rather than be adopted over")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the verdict stays Unknown — unproven never becomes ours or foreign")
	assert.Contains(t, err.Error(),
		"could not be read; the binding failed on "+unreadable+" — retry once "+unreadable+" is readable; inspect "+unreadable,
		"the exact adjacency: the transient clause follows the could-not-read text and the residue list lands on the inspect verb (#7900 R1)")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal still says the residue is unverified, not this session's retained original")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing over unproven residue must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released")
	assert.DirExists(t, unreadable,
		"the unreadable residue must be left untouched for the operator")
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed destination must be left in place")
	assert.Equal(t, "uncommitted", string(dirty))
}

// TestArchiveSession_UnverifiedResidueKeepsGenericWording pins the plain
// Unknown on the dest-absent arm (#7900 R1): a residue whose binding fails
// on something NOT in the unproven set — a malformed .git pointer — keeps
// the generic refusal wording, and the clause slot renders nothing: the
// text reads "could not be read; inspect <residue>" with no retry promise
// and no path where the clause belongs.
func TestArchiveSession_UnverifiedResidueKeepsGenericWording(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "plain-unknown")
	dest, err := archivedWorktreePath(repoID, "plain-unknown")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	// The recorded worktree is absent; a residue-shaped directory whose
	// .git pointer is unreadable-as-a-pointer (malformed content, not an
	// unproven errno) binds Unknown without the transient class.
	require.NoError(t, os.RemoveAll(wtPath))
	orphan := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("f", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, ".git"), []byte("not a gitdir pointer"), 0o644))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "plain-unknown", RepoID: repoID})
	require.Error(t, err, "an unverified residue must refuse fail-closed")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), "could not be read; inspect "+orphan,
		"the exact adjacency for a plain Unknown: no clause, the residue list lands directly on the inspect verb")
	assert.NotContains(t, err.Error(), "retry once",
		"a non-unproven Unknown never promises a retry")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.DirExists(t, orphan)
}

// TestArchiveSession_DestPresentUnverifiedQuarantineKeepsGenericWording pins
// the same adjacency on the dest-present arm (#7900 R1): a quarantine-shaped
// residue whose binding fails on a malformed pointer keeps the generic
// refusal wording — "could not be read; inspect <residue>" — with no
// transient clause in the slot.
func TestArchiveSession_DestPresentUnverifiedQuarantineKeepsGenericWording(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-plain")
	dest, err := archivedWorktreePath(repoID, "dest-plain")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	orphan := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("f", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, ".git"), []byte("not a gitdir pointer"), 0o644))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-plain", RepoID: repoID})
	require.Error(t, err,
		"an unverified quarantine-shaped residue must refuse rather than be adopted over")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), "could not be read; inspect "+orphan,
		"the exact adjacency for a plain Unknown: no clause, the residue list lands directly on the inspect verb")
	assert.NotContains(t, err.Error(), "retry once",
		"a non-unproven Unknown never promises a retry")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.DirExists(t, orphan)
}

// TestArchiveSession_DanglingAnchorSaysFixRemove pins #7900 R3 on the
// dest-absent arm: when the recorded source path is spelled through a
// dangling symlink, the residue's anchor comparison fails on the link — a
// component that is present but unresolvable, which IS unproven but is not
// a read a retry can complete. The refusal names the fix-or-remove remedy —
// "<component> is a dangling link: fix or remove it, or move the residue
// out" — and never promises a retry for a target that may be gone for good.
func TestArchiveSession_DanglingAnchorSaysFixRemove(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dangling-anchor")
	dest, err := archivedWorktreePath(repoID, "dangling-anchor")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	// A real worktree moved under the residue name — its .git pointer and
	// registration survive the raw rename, so the bind reaches the anchor
	// comparison, which fails on the dangling link the recorded path now
	// spells through.
	secondWt := filepath.Join(filepath.Dir(wtPath), "second-wt")
	gitOut(t, repoPath, "worktree", "add", "-b", "af/dangling-anchor-second", secondWt)
	residue := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("d", 32))
	require.NoError(t, os.Rename(secondWt, residue))
	require.NoError(t, os.RemoveAll(wtPath))
	require.NoError(t, os.Symlink(filepath.Join(filepath.Dir(wtPath), "nonexistent"), wtPath))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dangling-anchor", RepoID: repoID})
	require.Error(t, err, "an unproven residue must refuse fail-closed")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the verdict stays Unknown — unproven never becomes ours or foreign")
	assert.Contains(t, err.Error(),
		"could not be read; "+wtPath+" is a dangling link: fix or remove it, or move the residue out; inspect "+residue,
		"the exact adjacency: the dangling-link clause names the link and its remedy, and the residue list lands on the inspect verb (#7900 R1+R3)")
	assert.NotContains(t, err.Error(), "retry once",
		"a dangling link is itself readable — the retry promise is only for a read a retry can complete")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.DirExists(t, residue,
		"the residue must be left untouched for the operator")
}

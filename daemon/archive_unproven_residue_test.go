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
	assert.Contains(t, err.Error(), "retry once "+unreadable+" is readable",
		"the refusal must name the unreadable component and say the retry may clear once it is readable")
	assert.Contains(t, err.Error(), "the binding failed on "+unreadable,
		"the transient clause says WHY the binding failed")
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
	assert.Contains(t, err.Error(), "retry once "+unreadable+" is readable",
		"the refusal must name the unreadable component and say the retry may clear once it is readable")
	assert.Contains(t, err.Error(), "the binding failed on "+unreadable,
		"the transient clause says WHY the binding failed")
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

package daemon

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReserveCreate_ReusesArchivedName_RollbackRestoresRecordedPath is the
// legacy-archive rollback regression.
//
// sanitizeArchiveTitle's boundary is strict: a title exactly archiveLeafNameMax
// bytes now derives a digest-form leaf, but before the NAME_MAX bound the same
// title archived to the literal leaf. An archive written before the upgrade
// therefore sits on a path archivedWorktreePath(oldTitle) no longer produces.
//
// renameArchivedForReuseLocked relocates the archived worktree from wherever the
// record actually points, and its persist-failure rollback must return it to
// that same recorded path. Re-deriving the destination from the title instead
// lands the worktree on the new digest-form path while the durable row still
// claims the literal one — the create reports "rolled it back" while the record
// points at a directory that no longer exists.
func TestReserveCreate_ReusesArchivedName_RollbackRestoresRecordedPath(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// Exactly archiveLeafNameMax bytes: the pre-bound code stored this leaf
	// literally; the strict boundary now maps it to digest form.
	title := strings.Repeat("a", archiveLeafNameMax)
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, title, "legacy")

	// The seed archives to the CURRENT derivation (digest form for this title);
	// move the worktree to where a pre-upgrade archive would have put it — the
	// literal leaf — and repoint the in-memory worktree and the durable row, so
	// the session is indistinguishable from one archived before the bound.
	derivedDest, err := archivedWorktreePath(repoID, title)
	require.NoError(t, err)
	require.True(t, exists(derivedDest), "the seeded archived worktree must start at the derived archive path")
	legacyDest := filepath.Join(filepath.Dir(derivedDest), title)
	require.NotEqual(t, derivedDest, legacyDest,
		"the repro needs the recorded path to differ from the title's current derivation")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", derivedDest, legacyDest).CombinedOutput()
	require.NoError(t, err, string(out))
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, legacyDest, title, archived.GetBranch(), "", false, true)
	require.NoError(t, err)
	archived.SetGitWorktreeForTest(gw)
	require.NoError(t, persistInstanceData(repoID, archived.ToInstanceData()),
		"the durable row must claim the legacy path, as it would for a pre-upgrade archive")

	errForcedPersist := fmt.Errorf("forced durable-rename failure (legacy-path rollback repro)")
	seamFired := false
	prev := reuseArchivedRenamePersist
	reuseArchivedRenamePersist = func(rid, oldTitle string, newData session.InstanceData) error {
		seamFired = true
		return errForcedPersist
	}
	t.Cleanup(func() { reuseArchivedRenamePersist = prev })

	_, _, release, _, rerr := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath,
		Title:    title,
		Program:  "claude",
	})
	if release != nil {
		release()
	}

	require.True(t, seamFired, "the durable-rename seam never fired; the repro did not reach the rename path")
	require.Error(t, rerr, "the create must abort when the rename cannot be persisted")
	assert.ErrorIs(t, rerr, errForcedPersist, "the persist failure must be wrapped, not swallowed")
	assert.Contains(t, rerr.Error(), "rolled it back",
		"the error must report the rollback the way the operator sees it")

	// The rollback must restore the RECORDED path — the literal legacy leaf —
	// not the digest form the title would derive today. With the bug the
	// worktree lands on derivedDest while the row still claims legacyDest.
	assert.True(t, exists(legacyDest),
		"the rolled-back worktree must be back at the path the durable record claims")
	assert.Equal(t, legacyDest, archived.GetWorktreePath(),
		"the archived session's worktree path must be its recorded pre-rename path")
	assert.Equal(t, title, archived.Title)
	manager.mu.Lock()
	_, keyed := manager.instances[daemonInstanceKey(repoID, title)]
	manager.mu.Unlock()
	assert.True(t, keyed, "the archived row must be re-keyed under its restored title")
}

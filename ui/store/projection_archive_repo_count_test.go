package store

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

// newStartedRepoInstance builds a started local-worktree instance whose
// RepoName() resolves to repoName, mirroring newRepoInstance in
// ui/sidebar_test.go so the projection's repo bookkeeping (which drives the
// multi-repo indicator) can be exercised. GetRepoName() is
// filepath.Base(repoPath), so the worktree's repoPath ends in repoName.
func newStartedRepoInstance(t *testing.T, title, repoName string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	gw, err := git.NewGitWorktreeFromStorage(
		filepath.Join(t.TempDir(), repoName), // repoPath base = repoName
		filepath.Join(t.TempDir(), "worktree"),
		title,
		"branch",
		"deadbeef",
		false,
		true,
	)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// newColdStartArchivedInstance builds a record that loads inert (Started=false),
// like a session archived before this TUI session launched — the control case
// that already removes cleanly because it is never counted. FromInstanceData's
// archived path tries to restore a worktree, so this models the same
// projection-facing shape (local-worktree, Started=false, liveness=Archived)
// via NewInstance + SetArchived.
func newColdStartArchivedInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	require.False(t, inst.Started(), "cold-start archived fixture must load inert (Started=false)")
	inst.SetArchived()
	require.False(t, inst.Started())
	return inst
}

// TestProjectionArchiveRepoCount exercises the repo bookkeeping that drives the
// multi-repo indicator across the archive lifecycle. Archiving a local-worktree
// session flips Started=false in place on the same *session.Instance the
// projection already counted; the decrement paths must still drop the counted
// repo, rather than re-reading Started() and skipping the decrement. These
// subtests assert the fixed (expected) behavior and fail while the bug exists.
func TestProjectionArchiveRepoCount(t *testing.T) {
	// archive-then-remove_leaves_phantom_repo: archiving the only session in
	// repo-R and then removing it must drop repo-R from the bookkeeping. The
	// old code re-read Started() at removal time, saw the archived row as
	// "never counted", and skipped rmRepo — leaving repo-R registered so
	// NumRepos() never dropped and rows in other repos kept showing a
	// (repoName) annotation for the rest of the TUI session.
	t.Run("archive-then-remove_leaves_phantom_repo", func(t *testing.T) {
		p := NewProjection()
		inst := newStartedRepoInstance(t, "R", "repo-R")
		p.AddInstance(inst)() // finalize registers repo-R
		require.Equal(t, 1, p.NumRepos())
		_, hasR := p.repos["repo-R"]
		require.True(t, hasR)

		inst.SetArchived() // flips Started=false in place on the counted row
		require.False(t, inst.Started(), "archive must flip Started=false in place")
		require.True(t, p.ContainsInstance(inst))

		require.True(t, p.RemoveInstance(inst))
		_, hasR = p.repos["repo-R"]
		require.False(t, hasR,
			"repos[repo-R] still present after removing an archived row that counted; want absent")
		require.Equal(t, 0, p.NumRepos(),
			"NumRepos() = %d after removing the archived row; want 0 (phantom repo left behind)", p.NumRepos())
	})

	// archive-then-restore_double_counts: archiving then restoring the only
	// session in repo-R drives a ReplaceInstance (outgoing archived row →
	// fresh started replacement). The old code skipped the outgoing row's
	// rmRepo (re-read Started()==false) but still addRepo'd the replacement,
	// double-counting repo-R; a later removal decremented only once and left
	// repo-R behind. The fix captures the repo at registration time, so the
	// outgoing archived row decrements and the replacement increments, and a
	// later kill of the replacement drops repo-R cleanly.
	t.Run("archive-then-restore_double_counts", func(t *testing.T) {
		p := NewProjection()
		inst := newStartedRepoInstance(t, "R", "repo-R")
		p.AddInstance(inst)() // count repo-R = 1
		require.Equal(t, 1, p.NumRepos())

		inst.SetArchived() // archive flips Started=false in place on the counted row
		require.False(t, inst.Started())

		// Restore swaps the archived row for a fresh started instance of
		// the same repo (mirror of swapInstanceFromSnapshot).
		fresh := newStartedRepoInstance(t, "R2", "repo-R")
		require.True(t, p.ReplaceInstance(inst, fresh))
		require.Equal(t, 1, p.NumRepos(),
			"replace must balance (decrement outgoing archived, increment replacement); want 1, got double-count")
		_, hasR := p.repos["repo-R"]
		require.True(t, hasR)

		// A later delete (kill) of the replacement must drop repo-R.
		require.NoError(t, p.KillInstance(fresh))
		_, hasR = p.repos["repo-R"]
		require.False(t, hasR,
			"repos[repo-R] still present after killing the replacement; want absent")
		require.Equal(t, 0, p.NumRepos(),
			"NumRepos() = %d after killing the replacement; want 0 (double-counted repo-R left a phantom)", p.NumRepos())
	})

	// archive-then-kill_decrements: the kill spend path (used when D/delete is
	// offered on an archived row, or an archived row is killed directly) must
	// decrement the captured repo. The old code captured repoKeyOf before Kill,
	// read Started()==false on the archived row, and skipped the decrement.
	t.Run("archive-then-kill_decrements", func(t *testing.T) {
		p := NewProjection()
		inst := newStartedRepoInstance(t, "K", "repo-R")
		p.AddInstance(inst)() // count repo-R = 1
		require.Equal(t, 1, p.NumRepos())

		inst.SetArchived() // archive flips Started=false in place on the counted row
		require.False(t, inst.Started())

		require.NoError(t, p.KillInstance(inst))
		_, hasR := p.repos["repo-R"]
		require.False(t, hasR,
			"repos[repo-R] still present after killing an archived row that counted; want absent")
		require.Equal(t, 0, p.NumRepos(),
			"NumRepos() = %d after killing the archived row; want 0 (phantom repo left behind)", p.NumRepos())
		require.False(t, p.ContainsInstance(inst))
	})

	// cold-start_archived_record_never_counted (control): a record archived
	// before this TUI session launched loads inert (Started=false), is never
	// counted, and so must still remove cleanly. This is the behavior the
	// hot-archive path must match now that it captures the repo at
	// registration time.
	t.Run("cold-start_archived_record_never_counted", func(t *testing.T) {
		p := NewProjection()
		inst := newColdStartArchivedInstance(t, "archived")
		p.AddInstance(inst)() // finalize: archived ⇒ never counted ⇒ repoOf empty
		require.Equal(t, 0, p.NumRepos())
		_, hasR := p.repos["repo-R"]
		require.False(t, hasR)

		require.True(t, p.RemoveInstance(inst))
		require.False(t, p.ContainsInstance(inst))
		require.Equal(t, 0, p.NumRepos(),
			"removing a never-counted cold-start archived record must not change NumRepos()")
	})
}

// TestProjectionArchiveRepoCountMultiRepoAnnotation guards the user-visible
// implication: the multi-repo annotation `(repoName)` is rendered only when
// NumRepos() > 1. Archiving+removing the only session in one of two repos must
// drop NumRepos() to 1 so rows in the surviving repo stop showing the
// (repoName) annotation — the stale-indicator symptom the report describes.
func TestProjectionArchiveRepoCountMultiRepoAnnotation(t *testing.T) {
	p := NewProjection()
	a := newStartedRepoInstance(t, "a", "repo-A")
	b := newStartedRepoInstance(t, "b", "repo-B")
	p.AddInstance(a)()
	p.AddInstance(b)()
	require.Equal(t, 2, p.NumRepos(), "two repos ⇒ multi-repo annotation renders")

	// Archive then remove the only repo-A session.
	a.SetArchived()
	require.True(t, p.RemoveInstance(a))
	require.Equal(t, 1, p.NumRepos(),
		"archive→delete of repo-A's only session must drop NumRepos() to 1 so the annotation stops rendering")
	_, hasA := p.repos["repo-A"]
	require.False(t, hasA)
	_, hasB := p.repos["repo-B"]
	require.True(t, hasB)
}

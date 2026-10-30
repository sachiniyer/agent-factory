package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --apply tests (#5189). They use the real archive fixture: a git worktree that
// the daemon archived for real, so the nonce, the registration leaf and the kept
// branch are all genuine.

func applyReq(repoID string, ids ...string) PruneSessionsRequest {
	r := pruneReq(repoID)
	r.Apply = true
	r.Only = ids
	return r
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func tombstones(t *testing.T, archived string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(filepath.Dir(archived), ".prune-*"))
	require.NoError(t, err)
	return m
}

func planOf(t *testing.T, manager *Manager, repoID string) PruneCandidate {
	t.Helper()
	dry, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Len(t, dry.Candidates, 1, "skipped: %+v", dry.Skipped)
	require.NotEmpty(t, dry.Candidates[0].ID)
	return dry.Candidates[0]
}

func TestPruneApply_DeletesExactlyWhatTheDryRunListsAndKeepsTheBranch(t *testing.T) {
	manager, repoID, repoPath, inst, archived := seedPrunableArchive(t, "apply-me")
	c := planOf(t, manager, repoID)

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	require.Len(t, resp.Pruned, 1, "skipped=%+v incomplete=%+v", resp.Skipped, resp.Incomplete)
	assert.Empty(t, resp.Incomplete)
	assert.True(t, resp.Applied)

	assert.NoDirExists(t, archived)
	assert.Empty(t, tombstones(t, archived), "no renamed remains")
	gitOut(t, repoPath, "rev-parse", "--verify", "refs/heads/"+c.Branch) // the branch is kept
	assert.NotContains(t, gitOut(t, repoPath, "worktree", "list", "--porcelain"), archived, "the registration is gone")
	assert.True(t, inst.IsPruned())

	again, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	assert.Empty(t, again.Candidates)
	require.Len(t, again.Skipped, 1)
	assert.Contains(t, again.Skipped[0].Reason, "already pruned")
}

func TestPruneApply_IsBoundToTheConfirmedPlan(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "not-planned")
	_ = planOf(t, manager, repoID)

	resp, err := manager.PruneSessions(applyReq(repoID, "some-other-id"))
	require.NoError(t, err)
	assert.Empty(t, resp.Pruned)
	assert.DirExists(t, archived, "a row outside the confirmed plan is never touched")
	assert.False(t, inst.IsPruned())
	var reasons []string
	for _, s := range resp.Skipped {
		reasons = append(reasons, s.Reason)
	}
	assert.Contains(t, strings.Join(reasons, "|"), "not in the confirmed plan")
	assert.Contains(t, strings.Join(reasons, "|"), "no longer exists")
}

// Every refusal the dry run reports is also what apply honors, with the offending
// path named.
func TestPruneApply_RefusesWhatTheVerdictRefusesAndNamesThePath(t *testing.T) {
	type tc struct {
		name   string
		damage func(t *testing.T, archived string)
		want   []string
	}
	cases := []tc{
		{"assume-unchanged edit", func(t *testing.T, a string) {
			gitOut(t, a, "update-index", "--assume-unchanged", "dirty.txt")
			require.NoError(t, os.WriteFile(filepath.Join(a, "dirty.txt"), []byte("only copy"), 0o644))
		}, []string{"assume-unchanged", "dirty.txt"}},
		{"skip-worktree edit", func(t *testing.T, a string) {
			gitOut(t, a, "update-index", "--skip-worktree", "dirty.txt")
			require.NoError(t, os.WriteFile(filepath.Join(a, "dirty.txt"), []byte("only copy"), 0o644))
		}, []string{"skip-worktree", "dirty.txt"}},
		{"untracked file", func(t *testing.T, a string) {
			require.NoError(t, os.WriteFile(filepath.Join(a, "scratch.env"), []byte("secret"), 0o644))
		}, []string{"scratch.env"}},
		{"extra commit on a detached HEAD", func(t *testing.T, a string) {
			gitOut(t, a, "checkout", "--detach")
			require.NoError(t, os.WriteFile(filepath.Join(a, "extra.txt"), []byte("x"), 0o644))
			gitOut(t, a, "add", "extra.txt")
			gitOut(t, a, "commit", "-qm", "only here")
		}, []string{"kept branch"}},
		{"occupant nonce replaced", func(t *testing.T, a string) {
			gitDir := gitOut(t, a, "rev-parse", "--absolute-git-dir")
			require.NoError(t, os.WriteFile(filepath.Join(gitDir, "af-archive-nonce"), []byte("someone-else\n"), 0o600))
		}, []string{"nonce"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			manager, repoID, _, inst, archived := seedPrunableArchive(t, "refuse-"+strings.ReplaceAll(c.name, " ", "-"))
			c.damage(t, archived)

			dry, err := manager.PruneSessions(pruneReq(repoID))
			require.NoError(t, err)
			require.Empty(t, dry.Candidates, "the dry run must not list it")
			require.Len(t, dry.Skipped, 1)
			for _, w := range c.want {
				assert.Contains(t, dry.Skipped[0].Reason, w)
			}

			// Even a plan that names it (a stale one) deletes nothing.
			resp, err := manager.PruneSessions(applyReq(repoID, inst.ID))
			require.NoError(t, err)
			assert.Empty(t, resp.Pruned)
			assert.DirExists(t, archived)
			assert.Empty(t, tombstones(t, archived))
			assert.False(t, inst.IsPruned())
		})
	}
}

// A crash at each numbered step converges on re-run, and the worktree is never
// lost before the rename.
func TestPruneApply_CrashAtEveryStepConverges(t *testing.T) {
	type state struct{ original, tomb, intent bool }
	steps := []struct {
		crashAt string
		want    state
	}{
		{"intent", state{original: true}},
		{"rename", state{original: true, intent: true}},
		{"leaf", state{tomb: true, intent: true}},
		{"remove", state{tomb: true, intent: true}},
		{"tombstone", state{intent: true}},
	}
	for _, s := range steps {
		t.Run(s.crashAt, func(t *testing.T) {
			manager, repoID, repoPath, inst, archived := seedPrunableArchive(t, "crash-"+s.crashAt)
			c := planOf(t, manager, repoID)

			pruneStepHook = func(step string) error {
				if step == s.crashAt {
					return errPruneCrash
				}
				return nil
			}
			t.Cleanup(func() { pruneStepHook = nil })
			resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
			require.NoError(t, err)
			require.Len(t, resp.Incomplete, 1)
			assert.Equal(t, s.crashAt, resp.Incomplete[0].Step)
			assert.Equal(t, apiproto.ErrorCodeMutationCommitted, resp.Code, "an interrupted prune is a committed-but-incomplete outcome")

			_, statErr := os.Lstat(archived)
			assert.Equal(t, s.want.original, statErr == nil, "original present")
			assert.Equal(t, s.want.tomb, len(tombstones(t, archived)) == 1, "tombstone dir present")
			assert.Equal(t, s.want.intent, inst.PruneIntent() != nil, "intent recorded")
			assert.False(t, inst.IsPruned())

			// Re-run (no crash): converges, deleting only the intent's own path.
			pruneStepHook = nil
			resp, err = manager.PruneSessions(applyReq(repoID, c.ID))
			require.NoError(t, err)
			assert.Empty(t, resp.Incomplete)
			assert.True(t, inst.IsPruned(), "skipped=%+v", resp.Skipped)
			assert.Nil(t, inst.PruneIntent())
			assert.NoDirExists(t, archived)
			assert.Empty(t, tombstones(t, archived))
			gitOut(t, repoPath, "rev-parse", "--verify", "refs/heads/"+c.Branch)
		})
	}
}

// A hand-edited intent cannot point the deletion anywhere but a .prune- sibling.
func TestPruneApply_ResumeRefusesAnIntentThatPointsElsewhere(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "tampered")
	victim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, inst.SetPruneIntent(sessionIntent(archived, victim)))

	entry, inc := manager.resumePruneIntent(repoID, inst)
	assert.Nil(t, entry)
	require.NotNil(t, inc)
	assert.FileExists(t, filepath.Join(victim, "keep.txt"))
	assert.DirExists(t, archived)
}

func TestPruneApply_RestoreRefusesAPrunedSessionByNamingTheBranch(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "no-restore")
	c := planOf(t, manager, repoID)
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	require.True(t, inst.IsPruned())

	_, _, err = manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), c.Branch)
	assert.Contains(t, err.Error(), "pruned")
	assert.Equal(t, session.LifecycleActionNone, inst.LifecycleAction(), "the row offers no restore verb")
}

// A previous daemon's unfinished prune is completed at start-up, off the readiness
// path, from the intent alone.
func TestPruneApply_StartupResumeFinishesAnInterruptedPrune(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "startup-resume")
	c := planOf(t, manager, repoID)
	pruneStepHook = func(step string) error {
		if step == "remove" {
			return errPruneCrash
		}
		return nil
	}
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	pruneStepHook = nil
	require.Len(t, tombstones(t, archived), 1)

	var wg sync.WaitGroup
	launchPruneIntentResume(manager, make(chan struct{}), &wg)
	wg.Wait()
	assert.True(t, inst.IsPruned())
	assert.Empty(t, tombstones(t, archived))
}

// Dry run (no --apply) still writes nothing and rejects a stray plan binding.
func TestPruneApply_OnlyWithoutApplyIsRejected(t *testing.T) {
	manager, repoID, _, _, _ := seedPrunableArchive(t, "only-no-apply")
	req := pruneReq(repoID)
	req.Only = []string{"x"}
	_, err := manager.PruneSessions(req)
	require.Error(t, err)
}

func sessionIntent(worktree, tomb string) session.PruneIntentData {
	return session.PruneIntentData{WorktreePath: worktree, TombstonePath: tomb}
}

// P1 (Codex on #6186): after a crash between removing the registration and
// tombstoning, another worktree can occupy the original path (even under the same
// leaf name). Resume must not remove a registration it cannot prove is the
// recorded one, and must not touch the replacement.
func TestPruneApply_ResumeNeverRemovesAReoccupyingWorktreesRegistration(t *testing.T) {
	manager, repoID, repoPath, inst, archived := seedPrunableArchive(t, "reoccupied")
	c := planOf(t, manager, repoID)
	pruneStepHook = func(step string) error {
		if step == "remove" { // after the leaf is gone, before the tree is deleted
			return errPruneCrash
		}
		return nil
	}
	t.Cleanup(func() { pruneStepHook = nil })
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	pruneStepHook = nil
	require.Len(t, tombstones(t, archived), 1)

	// Another worktree takes the original path on the kept branch (its registration
	// may even reuse the original leaf's name).
	gitOut(t, repoPath, "worktree", "add", archived, c.Branch)
	require.NoError(t, os.WriteFile(filepath.Join(archived, "replacement.txt"), []byte("not ours"), 0o644))

	entry, inc := manager.resumePruneIntent(repoID, inst)
	_ = entry
	_ = inc
	assert.FileExists(t, filepath.Join(archived, "replacement.txt"), "the replacement worktree is untouched")
	assert.Contains(t, gitOut(t, repoPath, "worktree", "list", "--porcelain"), archived, "and its registration stays")
}

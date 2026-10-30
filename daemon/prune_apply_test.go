package daemon

import (
	"encoding/json"
	"errors"
	"github.com/sachiniyer/agent-factory/config"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		// A detached HEAD is refused by the occupant-identity check (the worktree no
		// longer sits on the session's branch) before the HEAD == branch-tip proof is
		// reached; that proof is exercised on its own in session/git
		// (TestWorktreeHeadMatchesBranch). The shape is still refused, and the
		// extra commit is still never deleted.
		{"extra commit on a detached HEAD", func(t *testing.T, a string) {
			gitOut(t, a, "checkout", "--detach")
			require.NoError(t, os.WriteFile(filepath.Join(a, "extra.txt"), []byte("x"), 0o644))
			gitOut(t, a, "add", "extra.txt")
			gitOut(t, a, "commit", "-qm", "only here")
		}, []string{"could not be verified as this session's worktree", "occupant"}},
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

// Free space is counted once per filesystem, not once per path (Codex on #6186).
func TestSumFreeCountsASharedFilesystemOnce(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(a, 0o755))
	require.NoError(t, os.Mkdir(b, 0o755))
	one, ok := sumFree([]string{a})
	require.True(t, ok)
	two, ok := sumFree([]string{a, b})
	require.True(t, ok)
	assert.Equal(t, one, two, "two archive roots on one mount are one filesystem")
}

// In-memory prune state moves forward only after the durable write that records it
// succeeds (Codex on #6186): a failed tombstone write leaves the intent in memory,
// the row reports incomplete, and the next apply in the SAME daemon completes it.
func TestPruneApply_FailedTombstoneWriteKeepsTheIntentForTheNextApply(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "tombstone-write-fails")
	c := planOf(t, manager, repoID)

	prev := pruneTombstonePersist
	pruneTombstonePersist = func(*Manager, string, session.InstanceData) error { return errors.New("disk full") }
	t.Cleanup(func() { pruneTombstonePersist = prev })

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	require.Len(t, resp.Incomplete, 1)
	assert.Equal(t, "tombstone", resp.Incomplete[0].Step)
	assert.Empty(t, resp.Pruned)
	assert.NoDirExists(t, archived, "the worktree is already deleted")
	assert.False(t, inst.IsPruned(), "memory did not move ahead of the failed write")
	require.NotNil(t, inst.PruneIntent(), "the intent is still there to resume from")

	pruneTombstonePersist = prev
	resp, err = manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Incomplete)
	require.Len(t, resp.Pruned, 1)
	assert.True(t, resp.Pruned[0].Resumed)
	assert.True(t, inst.IsPruned())
	assert.Nil(t, inst.PruneIntent())
}

// A worktree whose index holds a gitlink is never pruned, however the submodule
// looks (#5189): the dry run lists it as skipped, naming the path, and a stale plan
// that names it deletes nothing.
func TestPruneApply_NeverDeletesAWorktreeWithASubmodule(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "has-submodule")
	subRepo := t.TempDir()
	gitOut(t, subRepo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(subRepo, "f.txt"), []byte("x"), 0o644))
	gitOut(t, subRepo, "add", "-A")
	gitOut(t, subRepo, "commit", "-qm", "sub")
	gitOut(t, archived, "-c", "protocol.file.allow=always", "submodule", "add", subRepo, "mod")
	gitOut(t, archived, "commit", "-qm", "add submodule")

	dry, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, dry.Candidates)
	require.Len(t, dry.Skipped, 1)
	assert.Contains(t, dry.Skipped[0].Reason, "submodule")
	assert.Contains(t, dry.Skipped[0].Reason, "mod")

	resp, err := manager.PruneSessions(applyReq(repoID, inst.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Pruned)
	assert.DirExists(t, archived)
	assert.False(t, inst.IsPruned())
}

// Resume acts only on a positive answer (Codex on #6186): an unreadable archive
// parent makes both existence probes UNKNOWN, which must not read as "gone" and
// write the tombstone.
func TestPruneApply_ResumeWithUnknownExistenceStaysIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are bypassed by root")
	}
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "unknown-existence")
	c := planOf(t, manager, repoID)
	pruneStepHook = func(step string) error {
		if step == "remove" {
			return errPruneCrash
		}
		return nil
	}
	t.Cleanup(func() { pruneStepHook = nil })
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	pruneStepHook = nil
	require.NotNil(t, inst.PruneIntent())

	parent := filepath.Dir(archived)
	require.NoError(t, os.Chmod(parent, 0o000))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	entry, inc := manager.resumePruneIntent(repoID, inst)
	require.NoError(t, os.Chmod(parent, 0o755))
	assert.Nil(t, entry)
	require.NotNil(t, inc, "unknown is incomplete, not done")
	assert.False(t, inst.IsPruned())
	assert.NotNil(t, inst.PruneIntent(), "the intent stays")
	assert.Len(t, tombstones(t, archived), 1, "and the renamed tree was not touched")

	entry, inc = manager.resumePruneIntent(repoID, inst)
	assert.Nil(t, inc)
	require.NotNil(t, entry)
	assert.True(t, inst.IsPruned())
}

// The locked marker is part of the verdict: the dry run reports it and nothing is
// renamed or deleted.
func TestPruneApply_LockedRegistrationIsRefusedBeforeAnyMutation(t *testing.T) {
	manager, repoID, repoPath, inst, archived := seedPrunableArchive(t, "locked-reg")
	gitOut(t, repoPath, "worktree", "lock", archived)
	dry, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, dry.Candidates)
	require.Len(t, dry.Skipped, 1)
	assert.Contains(t, dry.Skipped[0].Reason, "locked")
	resp, err := manager.PruneSessions(applyReq(repoID, inst.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Pruned)
	assert.Empty(t, resp.Incomplete, "refused up front, not stranded after the rename")
	assert.DirExists(t, archived)
	assert.Nil(t, inst.PruneIntent())
}

// A kill while a prune is pending never orphans the renamed tree or the intent: it
// either completes the prune through the resume path or refuses with a message.
func TestPruneApply_KillDuringAPendingPruneNeverOrphansTheCleanup(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "kill-pending")
	c := planOf(t, manager, repoID)
	pruneStepHook = func(step string) error {
		if step == "remove" {
			return errPruneCrash
		}
		return nil
	}
	t.Cleanup(func() { pruneStepHook = nil })
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	pruneStepHook = nil
	require.Len(t, tombstones(t, archived), 1)

	_, killErr := manager.KillSession(KillSessionRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID})
	if killErr != nil {
		assert.Contains(t, killErr.Error(), "unfinished prune")
		assert.NotNil(t, inst.PruneIntent(), "refused: the intent is still there to finish")
		return
	}
	assert.Empty(t, tombstones(t, archived), "the kill finished the prune's cleanup instead of orphaning it")
}

// The tombstone's durable write and its in-memory commit are one critical section
// under the repository lock (Codex on #6186): a writer that arrives between the two
// steps must wait, and then persists the committed state, never the intent beside a
// missing worktree.
func TestPruneApply_TombstoneWriteAndMemoryCommitAreOneCriticalSection(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "tombstone-critical-section")
	c := planOf(t, manager, repoID)

	started, done := make(chan struct{}), make(chan struct{})
	pruneBetweenTombstoneSteps = func() {
		go func() { // the worktree-missing poll's checkpoint: persist after taking the repo lock
			close(started)
			_ = manager.persistInstanceErr(repoID, inst)
			close(done)
		}()
		<-started
		time.Sleep(100 * time.Millisecond) // give it every chance to run inside the window
	}
	t.Cleanup(func() { pruneBetweenTombstoneSteps = nil })

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	require.Len(t, resp.Pruned, 1, "incomplete=%+v", resp.Incomplete)
	<-done

	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	var rows []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &rows))
	var row *session.InstanceData
	for i := range rows {
		if rows[i].ID == inst.ID {
			row = &rows[i]
		}
	}
	require.NotNil(t, row)
	assert.False(t, row.PrunedAt.IsZero(), "disk holds the tombstone")
	assert.Nil(t, row.PruneIntent, "and not the intent beside a missing worktree")
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"

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
// The evidence is the device identity and the number of distinct filesystems
// summed — never two live statfs free-byte readings, which differ by a block
// whenever anything writes to the disk between them (#6226).
func TestSumFreeCountsASharedFilesystemOnce(t *testing.T) {
	const free = 4096 * 1000
	probe := func(devs map[string]uint64) func(string) (uint64, uint64, bool) {
		return func(path string) (uint64, uint64, bool) {
			dev, ok := devs[path]
			return free, dev, ok
		}
	}

	total, filesystems, ok := sumFreeWith([]string{"/a", "/b"}, probe(map[string]uint64{"/a": 7, "/b": 7}))
	require.True(t, ok)
	assert.Equal(t, 1, filesystems, "two archive roots on one mount are one filesystem")
	assert.EqualValues(t, free, total, "and its free space is added once")

	total, filesystems, ok = sumFreeWith([]string{"/a", "/b"}, probe(map[string]uint64{"/a": 7, "/b": 8}))
	require.True(t, ok)
	assert.Equal(t, 2, filesystems, "roots on different devices stay separate")
	assert.EqualValues(t, 2*free, total)

	_, _, ok = sumFreeWith([]string{"/a", "/missing"}, probe(map[string]uint64{"/a": 7}))
	assert.False(t, ok, "an unmeasurable root makes the whole sum unknown")
}

// The real probe reports one device for two directories of one temp dir. Device
// identity is stable between reads, unlike free space.
func TestFreeBytesAtReportsOneDeviceForSiblingDirectories(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Mkdir(a, 0o755))
	require.NoError(t, os.Mkdir(b, 0o755))
	_, devA, okA := freeBytesAt(a)
	_, devB, okB := freeBytesAt(b)
	if !okA || !okB {
		t.Skip("free space is not measurable on this platform")
	}
	assert.Equal(t, devA, devB)
	_, filesystems, ok := sumFreeWith([]string{a, b}, freeBytesAt)
	require.True(t, ok)
	assert.Equal(t, 1, filesystems)
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
// refuses with a message (the fence is checked inside the kill's own admission).
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
		assert.Contains(t, killErr.Error(), "pending or interrupted")
		assert.NotNil(t, inst.PruneIntent(), "refused: the intent is still there to finish")
		assert.Len(t, tombstones(t, archived), 1, "and the renamed tree is not orphaned or touched")
		return
	}
	t.Fatalf("a kill must not proceed while a prune intent is pending")
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

// Title reuse is fenced while a prune intent is pending (Codex on #6186): after a
// crash between intent and rename, reusing the archived title must not relocate
// the archive out from under the intent; once resume resolves it, reuse works.
func TestPruneApply_TitleReuseRefusesWhileAPruneIsPendingThenSucceedsAfterResume(t *testing.T) {
	manager, repoID, repoPath, inst, archived := seedPrunableArchive(t, "reuse-pending")
	c := planOf(t, manager, repoID)
	pruneStepHook = func(step string) error {
		if step == "rename" { // intent persisted, nothing moved yet
			return errPruneCrash
		}
		return nil
	}
	t.Cleanup(func() { pruneStepHook = nil })
	_, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	pruneStepHook = nil
	require.NotNil(t, inst.PruneIntent())

	disk, derr := loadRepoInstanceData(repoID)
	require.NoError(t, derr)
	manager.mu.Lock()
	renamed, rerr := manager.renameArchivedForReuseLocked(repoID, repoPath, "reuse-pending", "claude", runtimeNamespaceLocalTmux, &disk)
	manager.mu.Unlock()
	require.Error(t, rerr)
	assert.Contains(t, rerr.Error(), "pending or interrupted")
	assert.Nil(t, renamed)
	assert.DirExists(t, archived, "the archive was not relocated out from under the intent")
	assert.Equal(t, "reuse-pending", inst.Title)

	// Resume resolves the intent (the rename never happened: it is abandoned) ...
	entry, inc := manager.resumePruneIntent(repoID, inst)
	require.Nil(t, inc)
	require.Nil(t, entry)
	require.Nil(t, inst.PruneIntent())

	// ... and the title can then be reused.
	disk, derr = loadRepoInstanceData(repoID)
	require.NoError(t, derr)
	manager.mu.Lock()
	renamed, rerr = manager.renameArchivedForReuseLocked(repoID, repoPath, "reuse-pending", "claude", runtimeNamespaceLocalTmux, &disk)
	manager.mu.Unlock()
	require.NoError(t, rerr)
	require.NotNil(t, renamed)
}

// Lock order (Codex on #6186): a create takes the repository start lock before
// worktree admission. Prune must too — it must never hold admission while waiting
// for the start lock, or a prune overlapping a create deadlocks.
func TestPruneApply_TakesLocksInTheCreatesOrder(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "lock-order")
	c := planOf(t, manager, repoID)

	start := manager.startLockForRepo(repoID)
	start.Lock() // a create in this repository holds the start lock ...
	pruned := make(chan PruneSessionsResponse, 1)
	go func() {
		resp, _ := manager.PruneSessions(applyReq(repoID, c.ID))
		pruned <- resp
	}()
	time.Sleep(300 * time.Millisecond) // prune is now queued behind the start lock

	// ... and then takes worktree admission, as CreateSession does.
	admission := manager.worktreeAdmissionLockForRepo(repoID)
	got := make(chan struct{})
	go func() { admission.Lock(); close(got) }()
	deadlocked := false
	select {
	case <-got:
		admission.Unlock()
	case <-time.After(5 * time.Second):
		deadlocked = true
	}
	start.Unlock()
	if deadlocked {
		t.Fatal("deadlock: prune holds worktree admission while waiting for the start lock")
	}

	select {
	case resp := <-pruned:
		assert.Len(t, resp.Pruned, 1, "skipped=%+v incomplete=%+v", resp.Skipped, resp.Incomplete)
		assert.True(t, inst.IsPruned())
	case <-time.After(30 * time.Second):
		t.Fatal("prune did not complete once the create released its locks")
	}
}

// A row with an abandoned (timed-out) filesystem mutation is not acted on again
// until that mutation has finished.
func TestPruneApply_RefusesARowWithAnAbandonedMutationUntilItFinishes(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "abandoned-mutation")
	c := planOf(t, manager, repoID)

	release := make(chan struct{})
	finished := make(chan struct{})
	err := sessiongit.BoundedMutationWithin(50*time.Millisecond, "renaming the archive", []string{archived}, func() error {
		<-release
		close(finished)
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Pruned)
	require.NotEmpty(t, resp.Skipped)
	assert.Contains(t, resp.Skipped[0].Reason, "may still be running")
	assert.DirExists(t, archived)
	assert.Nil(t, inst.PruneIntent())

	close(release)
	<-finished
	require.Eventually(t, func() bool {
		r, _ := manager.PruneSessions(applyReq(repoID, c.ID))
		return len(r.Pruned) == 1
	}, 10*time.Second, 50*time.Millisecond, "after the abandoned mutation finished the prune proceeds")
}

// A write of the session records that timed out has an unknown outcome; prune does
// not act on the repository's rows again until it finishes.
func TestPruneApply_RefusesWhileAnAbandonedRecordWriteMayStillBeRunning(t *testing.T) {
	manager, repoID, _, _, archived := seedPrunableArchive(t, "abandoned-persist")
	c := planOf(t, manager, repoID)
	release := make(chan struct{})
	finished := make(chan struct{})
	err := sessiongit.BoundedMutationWithin(50*time.Millisecond, "saving the session records",
		[]string{prunePersistKey(repoID)}, func() error { <-release; close(finished); return nil })
	require.ErrorIs(t, err, context.DeadlineExceeded)

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Pruned)
	require.NotEmpty(t, resp.Skipped)
	assert.Contains(t, resp.Skipped[0].Reason, "may still be running")
	assert.DirExists(t, archived)
	close(release)
	<-finished
}

// P1 (Codex on #6186): an intent write that times out has an UNKNOWN outcome. The
// intent stays in memory and the abandoned write is registered, so a restore
// cannot succeed and then be overwritten by the delayed write carrying the intent.
func TestPruneApply_DelayedIntentWriteThenRestoreIsRefusedAndConverges(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "delayed-intent")
	c := planOf(t, manager, repoID)

	prevTimeout, prevImpl := prunePersistTimeout, prunePersistImpl
	prunePersistTimeout = 50 * time.Millisecond
	prunePersistImpl = func(repoID string, data session.InstanceData) error {
		time.Sleep(500 * time.Millisecond) // the write lands LATE
		return persistInstanceData(repoID, data)
	}
	t.Cleanup(func() { prunePersistTimeout, prunePersistImpl = prevTimeout, prevImpl })

	resp, err := manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	require.Len(t, resp.Incomplete, 1)
	assert.Equal(t, "intent", resp.Incomplete[0].Step)
	require.NotNil(t, inst.PruneIntent(), "the intent is not cleared on an unknown outcome")
	assert.DirExists(t, archived, "nothing was renamed")

	// Restore, kill and a new prune all refuse while the row is fenced.
	_, _, rerr := manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID})
	require.Error(t, rerr)
	assert.Contains(t, rerr.Error(), "pending or interrupted")
	_, kerr := manager.KillSession(KillSessionRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID})
	require.Error(t, kerr)
	assert.DirExists(t, archived)

	// The delayed write lands; the system then converges from the record on disk.
	prunePersistImpl, prunePersistTimeout = prevImpl, prevTimeout
	require.Eventually(t, func() bool { return sessiongit.MutationInFlight(prunePersistKey(repoID)) == nil }, 5*time.Second, 20*time.Millisecond)
	resp, err = manager.PruneSessions(applyReq(repoID, c.ID))
	require.NoError(t, err)
	assert.Empty(t, resp.Incomplete, "skipped=%+v", resp.Skipped)
	assert.True(t, inst.IsPruned())
	assert.NoDirExists(t, archived)
}

// The shared fence consults both halves: with the intent cleared in memory, a
// pending abandoned write still refuses restore.
func TestPruneApply_FenceCoversAnAbandonedWriteEvenWithoutAnIntent(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "fence-both-halves")
	release := make(chan struct{})
	finished := make(chan struct{})
	require.ErrorIs(t, sessiongit.BoundedMutationWithin(50*time.Millisecond, "saving the session records",
		[]string{prunePersistKey(repoID)}, func() error { <-release; close(finished); return nil }), context.DeadlineExceeded)
	require.Nil(t, inst.PruneIntent())

	_, _, err := manager.RestoreSession(RestoreSessionRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may still be running")
	close(release)
	<-finished
}

// Prune acts only on direct children of the repo's archive root (<AF home>/archived/
// <repoID>), in apply AND in resume (the last gap in the design note). A corrupted or
// hand-edited intent that aims outside it refuses and leaves the intent for a human.
func TestPruneUnderArchiveRoot(t *testing.T) {
	_, repoID, _, _, archived := seedPrunableArchive(t, "under-root")
	root, err := pruneArchiveRoot(repoID)
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(archived), root, "premise: the archive lives directly under the root")

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))

	require.NoError(t, pruneUnderArchiveRoot(repoID, archived), "a direct child")
	require.NoError(t, pruneUnderArchiveRoot(repoID, filepath.Join(root, ".prune-abc-123")), "a tombstone sibling, even before it exists")
	for name, p := range map[string]string{
		"grandchild":                   filepath.Join(archived, "inner"),
		"the root itself":              root,
		"a sibling repo's root":        filepath.Join(filepath.Dir(root), "other-repo", "wt"),
		"anywhere else":                filepath.Join(outside, ".prune-x"),
		"through a symlinked dir":      filepath.Join(root, "escape", ".prune-x"),
		"relative":                     ".prune-x",
		"dotdot that cleans to inside": root + "/sub/../.prune-x",
		"dotdot that escapes":          root + "/../" + filepath.Base(outside) + "/.prune-x",
		"duplicate separators":         root + "//.prune-x",
		"trailing slash":               root + "/.prune-x/",
		"dot element":                  root + "/./.prune-x",
		"empty":                        "",
	} {
		assert.Error(t, pruneUnderArchiveRoot(repoID, p), name)
	}
}

func TestPruneApply_ResumeRefusesAnIntentOutsideTheArchiveRoot(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "outside-root")
	outside := t.TempDir()
	victim := filepath.Join(outside, ".prune-victim")
	require.NoError(t, os.MkdirAll(victim, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("not ours"), 0o644))

	for name, intent := range map[string]session.PruneIntentData{
		"worktree and tombstone outside": {WorktreePath: filepath.Join(outside, "wt"), TombstonePath: victim},
		"only the worktree outside":      {WorktreePath: filepath.Join(outside, "wt"), TombstonePath: filepath.Join(filepath.Dir(archived), ".prune-ok")},
		"only the tombstone outside":     {WorktreePath: archived, TombstonePath: victim},
		// Cleans to the victim outside the root, while looking like a child of it as written.
		"a dotdot tombstone escaping after cleaning": {WorktreePath: archived,
			TombstonePath: filepath.Dir(archived) + "/../../../.." + victim},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, inst.SetPruneIntent(intent))
			entry, inc := manager.resumePruneIntent(repoID, inst)
			assert.Nil(t, entry)
			require.NotNil(t, inc, "the row reports an error")
			assert.NotNil(t, inst.PruneIntent(), "the intent stays for a human")
			assert.False(t, inst.IsPruned())
			assert.FileExists(t, filepath.Join(victim, "keep.txt"), "nothing outside the archive root was deleted")
			assert.DirExists(t, archived)
			inst.ClearPruneIntent()
		})
	}

	// A parent that is a symlink out of the root is outside too.
	root, err := pruneArchiveRoot(repoID)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	require.NoError(t, inst.SetPruneIntent(session.PruneIntentData{
		WorktreePath: filepath.Join(root, "escape", "wt"), TombstonePath: filepath.Join(root, "escape", ".prune-victim"),
	}))
	_, inc := manager.resumePruneIntent(repoID, inst)
	require.NotNil(t, inc)
	assert.FileExists(t, filepath.Join(victim, "keep.txt"))
}

// An invalid intent is surfaced, never silently discarded (Codex on #6186): even in
// the branch where the original still exists, the tombstone is absent and nothing was
// renamed — the case resume would otherwise abandon by clearing the intent.
func TestPruneApply_ResumeLeavesAnInvalidIntentInPlaceEvenWhenTheOriginalExists(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "invalid-intent-abandon")
	root := filepath.Dir(archived)
	for name, intent := range map[string]session.PruneIntentData{
		"tombstone outside the root":      {WorktreePath: archived, TombstonePath: filepath.Join(t.TempDir(), ".prune-x")},
		"unclean tombstone":               {WorktreePath: archived, TombstonePath: root + "/sub/../.prune-x"},
		"tombstone not a .prune- sibling": {WorktreePath: archived, TombstonePath: filepath.Join(root, "not-a-tombstone")},
		"relative worktree path":          {WorktreePath: "relative/wt", TombstonePath: filepath.Join(root, ".prune-x")},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, inst.SetPruneIntent(intent)) // not renamed, tombstone absent, original exists
			require.NoError(t, manager.persistInstanceErr(repoID, inst))
			entry, inc := manager.resumePruneIntent(repoID, inst)
			assert.Nil(t, entry)
			require.NotNil(t, inc, "the row is reported as needing a human")
			assert.Contains(t, inc.Reason, "invalid")
			require.NotNil(t, inst.PruneIntent(), "the invalid intent was NOT discarded")
			assert.DirExists(t, archived)
			// And it is still on disk, not only in memory.
			raw, err := config.LoadRepoInstances(repoID)
			require.NoError(t, err)
			assert.Contains(t, string(raw), "prune_intent")
			inst.ClearPruneIntent()
		})
	}
}

// Lock order against restore (Codex on #6186): restore takes worktree admission and
// then persists (the start lock); create takes the start lock and then admission.
// Prune takes NO admission, so a prune overlapping a restore-order holder cannot
// form a cycle with it. Under a deadline: the holder acquires admission then the
// start lock, exactly as restore does, while a prune is queued.
func TestPruneApply_DoesNotFormALockCycleWithRestoreOrder(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "restore-order")
	c := planOf(t, manager, repoID)

	admission := manager.worktreeAdmissionLockForRepo(repoID)
	admission.Lock() // restore holds admission ...
	pruned := make(chan PruneSessionsResponse, 1)
	go func() {
		resp, _ := manager.PruneSessions(applyReq(repoID, c.ID))
		pruned <- resp
	}()
	persisted := make(chan error, 1)
	go func() { // ... and then persists, which takes the start lock
		time.Sleep(200 * time.Millisecond)
		persisted <- manager.persistInstanceErr(repoID, inst)
	}()

	deadline := time.After(15 * time.Second)
	var resp PruneSessionsResponse
	deadlocked := false
	for got := 0; got < 2 && !deadlocked; got++ {
		select {
		case resp = <-pruned:
		case <-persisted:
		case <-deadline:
			deadlocked = true
		}
	}
	admission.Unlock()
	if deadlocked {
		t.Fatal("deadlock: prune and a restore-order holder wait on each other")
	}
	assert.Len(t, resp.Pruned, 1, "skipped=%+v incomplete=%+v", resp.Skipped, resp.Incomplete)
}

// The start-up resume shares ONE budget: when it is spent the pass stops cleanly,
// leaving the intent and the renamed tree in place, and a later pass with budget
// finishes it. It runs off the readiness path, so daemon start never waits on it.
func TestPruneApply_StartupResumeStopsCleanlyWhenItsBudgetIsSpent(t *testing.T) {
	manager, repoID, _, inst, archived := seedPrunableArchive(t, "budget-spent")
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

	prev := pruneResumeBudget
	pruneResumeBudget = time.Nanosecond // already spent when the pass starts
	t.Cleanup(func() { pruneResumeBudget = prev })
	var wg sync.WaitGroup
	start := time.Now()
	launchPruneIntentResume(manager, make(chan struct{}), &wg)
	assert.Less(t, time.Since(start), 2*time.Second, "launching never blocks the caller (daemon readiness)")
	wg.Wait()
	assert.NotNil(t, inst.PruneIntent(), "the intent is left in place")
	assert.False(t, inst.IsPruned())
	assert.Len(t, tombstones(t, archived), 1, "and so is the renamed tree")

	pruneResumeBudget = prev
	launchPruneIntentResume(manager, make(chan struct{}), &wg)
	wg.Wait()
	assert.True(t, inst.IsPruned(), "a pass with budget finishes it")
	assert.Empty(t, tombstones(t, archived))
}

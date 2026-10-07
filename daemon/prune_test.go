package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitFixtureDirty commits the uncommitted file registerArchivable writes,
// so the resulting worktree is CLEAN at archive time. Prune's listing refuses
// an archived worktree that still carries uncommitted content — it is the
// only copy, and the kept branch does not contain it (#5136 review) — so
// every fixture that wants a reclaimable row needs this first.
func commitFixtureDirty(t *testing.T, wtPath string) {
	t.Helper()
	out, err := exec.Command("git", "-C", wtPath, "add", "-A").CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", wtPath, "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "-qm", "commit fixture content").CombinedOutput()
	require.NoError(t, err, string(out))
}

// seedPrunableArchive registers a session and archives it for real — the same
// fixture the archive tests use — then lets the stamped archive time age past
// the smallest admissible cutoff ("1ms"), which a 5ms sleep buys without
// reaching for a private setter.
func seedPrunableArchive(t *testing.T, title string) (*Manager, string, string, *session.Instance, string) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, title)
	commitFixtureDirty(t, wtPath)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(archivedPath, "dirty.txt"))
	// Let the archive timestamp age past the tiniest admissible cutoff.
	time.Sleep(5 * time.Millisecond)
	return manager, repoID, repoPath, inst, archivedPath
}

func pruneReq(repoID string) PruneSessionsRequest {
	return PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms"}
}

// treeFingerprint hashes EVERYTHING under root — every directory, every
// file's mode+contents, every symlink's target — into one map keyed by
// relative path. Comparing two fingerprints proves no file was created,
// removed, renamed, or rewritten (#5136 slice 1: the dry run must write
// NOTHING).
func treeFingerprint(t *testing.T, root string) map[string]string {
	t.Helper()
	fp := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		h := sha256.New()
		info, err := os.Lstat(path)
		require.NoError(t, err)
		h.Write([]byte(info.Mode().String()))
		h.Write([]byte{0})
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			require.NoError(t, err)
			h.Write([]byte("L" + target))
		} else if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			require.NoError(t, err)
			h.Write([]byte("F"))
			h.Write(body)
		} else {
			h.Write([]byte("D"))
		}
		fp[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	require.NoError(t, err)
	return fp
}

// TestPruneSessions_DryRunListsCandidate: the listing names the reclaimable
// row, its branch, and the ALLOCATED bytes it would free.
func TestPruneSessions_DryRunListsCandidate(t *testing.T) {
	manager, repoID, _, _, archivedPath := seedPrunableArchive(t, "dry-run-row")

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Len(t, resp.Candidates, 1)
	entry := resp.Candidates[0]
	assert.Equal(t, "dry-run-row", entry.Title)
	assert.Equal(t, "af/dry-run-row", entry.Branch)
	assert.Greater(t, entry.ReclaimableBytes, int64(0))
	assert.Equal(t, entry.ReclaimableBytes, resp.ReclaimableBytes)

	assert.True(t, exists(archivedPath), "a dry run must not touch the archived worktree")
}

// TestPruneSessions_DryRunWritesNothing is the slice-1 contract (#5136
// review): the whole scan is a pure read, so the AF home AND the archive
// tree — and the repo's own .git, which a plain `git status` would silently
// refresh — must be byte-identical before and after.
func TestPruneSessions_DryRunWritesNothing(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath := seedPrunableArchive(t, "pristine-row")
	afHome := os.Getenv("AGENT_FACTORY_HOME")
	require.NotEmpty(t, afHome)
	archiveRoot := filepath.Dir(archivedPath)

	before := map[string]map[string]string{
		"af-home":     treeFingerprint(t, afHome),
		"archive-dir": treeFingerprint(t, archiveRoot),
		"repo":        treeFingerprint(t, repoPath),
	}

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Len(t, resp.Candidates, 1, "the seeded archive must list as reclaimable")

	for name, snap := range before {
		var root string
		switch name {
		case "af-home":
			root = afHome
		case "archive-dir":
			root = archiveRoot
		case "repo":
			root = repoPath
		}
		assert.Equal(t, snap, treeFingerprint(t, root),
			"the dry run must leave %s byte-identical", name)
	}
}

// TestPruneSessions_SkipsIneligibleRows: live rows and too-recent archives are
// reported with reasons — never silently filtered.
func TestPruneSessions_SkipsIneligibleRows(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// An archived row old enough to take the cutoff — seeded FIRST because
	// ArchiveSession refreshes the in-memory map from disk, and the test
	// helpers rewrite the repo file with only the row they register.
	oldInst, oldWt := registerArchivable(t, manager, repoID, repoPath, "old-row")
	commitFixtureDirty(t, oldWt)
	oldInst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "old-row", RepoID: repoID})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	// A live row in the same repo, registered after the archive refresh —
	// in m.instances ONLY. registerStarted's seedDiskInstance REWRITES the
	// repo file with just the row it registers, which would evict old-row's
	// durable record (#5136 review).
	liveInst, err := session.NewInstance(session.InstanceOptions{Title: "live-row", Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	liveInst.SetBackend(session.NewFakeBackend())
	liveInst.SetStartedForTest(true)
	liveInst.SetStatusForTest(session.Running)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "live-row")] = liveInst
	manager.mu.Unlock()

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)

	var titles []string
	for _, e := range resp.Candidates {
		titles = append(titles, e.Title)
	}
	assert.ElementsMatch(t, []string{"old-row"}, titles)

	skips := make(map[string]string)
	for _, s := range resp.Skipped {
		skips[s.Title] = s.Reason
	}
	require.Contains(t, skips, "live-row")
	assert.Contains(t, skips["live-row"], "not archived")

	// A row archived moments ago under a tight cutoff reports "too recent".
	young, _ := registerArchivable(t, manager, repoID, repoPath, "young-row")
	young.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	archivedYoung, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "young-row", RepoID: repoID})
	require.NoError(t, err)

	resp2, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "24h"})
	require.NoError(t, err)
	require.Empty(t, resp2.Candidates)
	skips2 := make(map[string]string)
	for _, s := range resp2.Skipped {
		skips2[s.Title] = s.Reason
	}
	require.Contains(t, skips2, "young-row")
	assert.Contains(t, skips2["young-row"], "archived too recently")
	assert.True(t, exists(archivedYoung), "a too-recent archive must be left alone")
}

// TestPruneSessions_RefusesRecycledPathOccupant: the record's pathname is not
// identity — if the archived worktree was removed out-of-band and an
// unrelated directory now occupies the path, the listing must refuse rather
// than count the replacement as reclaimable (#5136 review).
func TestPruneSessions_RefusesRecycledPathOccupant(t *testing.T) {
	manager, repoID, _, _, archivedPath := seedPrunableArchive(t, "recycled-row")

	// Delete the real archive and park a foreign directory at its pathname.
	require.NoError(t, os.RemoveAll(archivedPath))
	require.NoError(t, os.MkdirAll(filepath.Join(archivedPath, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archivedPath, "sub", "keep.txt"), []byte("not af's"), 0o644))

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "could not be verified")

	assert.True(t, exists(filepath.Join(archivedPath, "sub", "keep.txt")),
		"the foreign occupant must be left untouched")
}

// TestPruneSessions_RefusesDirtyArchivedWorktree: the local archive relocates
// bytes verbatim, so uncommitted content in the archived worktree is the ONLY
// copy — a reclaim would destroy work the kept branch does not contain
// (#5136 review). The row must list as refused, not reclaimable.
func TestPruneSessions_RefusesDirtyArchivedWorktree(t *testing.T) {
	manager, repoID, _, _, archivedPath := seedPrunableArchive(t, "dirty-row")
	require.NoError(t, os.WriteFile(
		filepath.Join(archivedPath, "uncommitted.txt"), []byte("the only copy"), 0o644))

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "uncommitted")
	assert.True(t, exists(filepath.Join(archivedPath, "uncommitted.txt")),
		"the uncommitted file is the only copy — it must survive")
}

// TestPruneSessions_RefusesIgnoredFile: an IGNORED file never shows in a
// plain porcelain status yet is just as unrecoverable — a gitignored .env
// lives nowhere but the archived worktree, and the kept branch cannot
// restore what it never tracked (#5136 Codex round 5). The dirty gate must
// count it.
func TestPruneSessions_RefusesIgnoredFile(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "secret-row")
	// Commit the ignore rule BEFORE the ignored file is written: git add -A
	// skips ignored paths, so secret.env stays ignored-and-uncommitted and
	// the committed rule is what the archive's status will honor.
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, ".gitignore"), []byte("secret.env\n"), 0o644))
	commitFixtureDirty(t, wtPath)
	require.NoError(t, os.WriteFile(filepath.Join(wtPath, "secret.env"), []byte("TOKEN=x"), 0o644))
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "secret-row", RepoID: repoID})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(archivedPath, "secret.env"))
	time.Sleep(5 * time.Millisecond)

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "uncommitted or ignored")
	assert.True(t, exists(filepath.Join(archivedPath, "secret.env")),
		"the only copy of an ignored file must survive")
}

// TestPruneSessions_RefusesSameRepoReplacementOccupant: the repo-present
// pointer binding alone proves only that the occupant belongs to the same
// REPO — a different worktree of that repo parked at a recycled archived path
// satisfies it. Counting it reclaimable would price destroying another
// session's checkout, so the occupant's registered branch must also match
// this session's recorded branch (#5136 Codex round 2).
func TestPruneSessions_RefusesSameRepoReplacementOccupant(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath := seedPrunableArchive(t, "replaced-row")

	// The real archive is gone and a same-repo worktree on a DIFFERENT branch
	// now occupies its recorded path — exactly the replacement scenario.
	require.NoError(t, os.RemoveAll(archivedPath))
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add",
		"-b", "af/someone-else", archivedPath).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(
		filepath.Join(archivedPath, "keep.txt"), []byte("another session's checkout"), 0o644))

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "could not be verified")
	assert.True(t, exists(filepath.Join(archivedPath, "keep.txt")),
		"the replacement worktree is not this session's — it must be left untouched")
}

// TestPruneSessions_RefusesRecreatedWorktree is the finding the branch check
// alone cannot cover: repo, path, AND branch are all reusable spellings —
// after the original archive is deleted, `git worktree add <path> <branch>`
// recreates a clean linked worktree that satisfies every comparison while
// being a different worktree. What it cannot recreate is the registration
// leaf's age, so the leaf must predate the session's archive time — a fresh
// leaf means refuse and leave the replacement's contents alone (#5136 Codex
// round 4).
func TestPruneSessions_RefusesRecreatedWorktree(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath := seedPrunableArchive(t, "rebuilt-row")

	// Delete the original archive, drop its stale registration, and recreate
	// a clean linked worktree at the same path on the same retained branch —
	// the exact reproduction shape the review describes.
	require.NoError(t, os.RemoveAll(archivedPath))
	// --expire=now so the fresh stale registration is dropped on every git
	// vintage — without it some versions only reap entries older than
	// gc.worktreePruneExpire and the re-add below fails "already registered"
	// (#5136 Codex round 5).
	out, err := exec.Command("git", "-C", repoPath, "worktree", "prune", "--expire=now").CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command("git", "-C", repoPath, "worktree", "add",
		archivedPath, "af/rebuilt-row").CombinedOutput()
	require.NoError(t, err, string(out))
	require.FileExists(t, filepath.Join(archivedPath, ".git"),
		"the recreation really is a registered linked worktree at the same path")

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "could not be verified")
	assert.True(t, exists(archivedPath),
		"the recreated worktree is not the archived original — it must be left untouched")
}

// TestPruneSessions_RefusesRepoGoneOrigin: when the origin repository is
// deleted there is no reachable branch to satisfy the kept-branch promise —
// the archived directory may be the last copy of the work, and the listing
// must refuse rather than price it as reclaimable (#5136 Codex round 3).
func TestPruneSessions_RefusesRepoGoneOrigin(t *testing.T) {
	manager, repoID, repoPath, _, archivedPath := seedPrunableArchive(t, "gone-row")
	require.NoError(t, os.RemoveAll(repoPath))

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "origin repository")
	assert.True(t, exists(archivedPath),
		"with the repo gone the archived directory may be the last copy — it must survive")
}

// TestPruneSessions_ConcurrentArchiveIsSkipped: a session mid-archive (its
// in-flight op legitimately raised by BeginKill) is exactly the race the op
// gate exists for — the listing must report it, not count it reclaimable.
func TestPruneSessions_ConcurrentArchiveIsSkipped(t *testing.T) {
	manager, repoID, _, inst, archivedPath := seedPrunableArchive(t, "racy-row")
	require.NoError(t, inst.Transition(session.BeginKill()))
	defer inst.Transition(session.RevertKill()) //nolint:errcheck

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "operation in flight")
	assert.True(t, exists(archivedPath), "nothing may be touched under a held lifecycle op")
}

// TestPruneSessions_KillsInFlightClaimIsSkipped: the daemon's exclusive
// lifecycle claim (kill/restore/delete-project) is honored alongside the
// op flag — a row another operation owns reports busy.
func TestPruneSessions_KillsInFlightClaimIsSkipped(t *testing.T) {
	manager, repoID, _, _, archivedPath := seedPrunableArchive(t, "claimed-row")
	key := daemonInstanceKey(repoID, "claimed-row")
	manager.mu.Lock()
	manager.killsInFlight[key] = struct{}{}
	manager.mu.Unlock()

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.Empty(t, resp.Candidates)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "in progress")
	assert.True(t, exists(archivedPath))
}

// TestPruneSessions_RequiresScopeAndDuration: the validation that keeps the
// command honest — no retention guess, no unscoped run.
func TestPruneSessions_RequiresScopeAndDuration(t *testing.T) {
	manager, _, _, _, _ := seedPrunableArchive(t, "guard-row")

	_, err := manager.PruneSessions(PruneSessionsRequest{RepoID: "x"})
	require.ErrorContains(t, err, "older_than is required")

	_, err = manager.PruneSessions(PruneSessionsRequest{RepoID: "x", OlderThan: "not-a-duration"})
	require.ErrorContains(t, err, "not a positive Go duration")

	_, err = manager.PruneSessions(PruneSessionsRequest{RepoID: "x", OlderThan: "-1h"})
	require.ErrorContains(t, err, "not a positive Go duration")

	_, err = manager.PruneSessions(PruneSessionsRequest{OlderThan: "1ms"})
	require.ErrorContains(t, err, "a scope is required")

	// Both scopes set: the control-RPC contract matches the CLI's mutual
	// exclusion rather than silently reporting on one repo while listing
	// cross-repo skips.
	_, err = manager.PruneSessions(PruneSessionsRequest{RepoID: "x", All: true, OlderThan: "1ms"})
	require.ErrorContains(t, err, "mutually exclusive")
}

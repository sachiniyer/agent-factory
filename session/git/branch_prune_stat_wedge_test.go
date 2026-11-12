package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The filesystem half of the bounded branch-prune path (#7111, PR #7118).
//
// Bounding the git calls is not enough on its own. Before localBranchExists
// runs any git, it stats the repo root, stats <root>/.git and, when there is
// no .git, walks the bare-layout probe (HEAD, config, objects, refs). On the
// stalled NFS/FUSE mount the git bound exists for, any of those stats blocks
// just as hard, and `af reset` never reaches the bounded git at all. These
// tests stall the STAT seam rather than git, at each of those observations.

// stallStatAt makes every BoundedStat of path block until the test ends, the
// way a stat on a dead mount does. Other paths take the real syscall.
func stallStatAt(t *testing.T, path string) {
	t.Helper()
	original := boundedStatPath
	release := make(chan struct{})
	boundedStatPath = func(p string) (os.FileInfo, error) {
		if p == path {
			<-release
		}
		return original(p)
	}
	// Registered first so LIFO runs it last, after the drain below; a t.Fatal
	// inside that drain cannot skip a separately registered cleanup (#4160).
	t.Cleanup(func() { boundedStatPath = original })
	t.Cleanup(func() {
		close(release)
		key := boundedStatKey{path: path, follow: true}
		deadline := time.Now().Add(5 * time.Second)
		for {
			boundedLstatFlights.Lock()
			flight := boundedLstatFlights.byPath[key]
			boundedLstatFlights.Unlock()
			if flight == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the stalled stat flight did not drain after release")
			}
			time.Sleep(time.Millisecond)
		}
	})
}

// THE REGRESSION. A stat that never returns, at any of the observations that
// precede the bounded git, must surface as a bounded ErrBranchDeletionTimedOut,
// so commands/reset.go keeps the session record exactly as it does for a
// stalled git. It must never hang, and never read as "absent".
func TestDeleteLocalBranchStalledStatDoesNotWedgeOrFabricateAbsence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repo  func(t *testing.T, branch string) string
		stall func(root string) string
	}{
		{"repo root", branchRepo, func(root string) string { return root }},
		{".git", branchRepo, func(root string) string { return filepath.Join(root, ".git") }},
		{"bare probe HEAD", bareCloneRepo, func(root string) string { return filepath.Join(root, "HEAD") }},
		{"bare probe config", bareCloneRepo, func(root string) string { return filepath.Join(root, "config") }},
		{"bare probe objects", bareCloneRepo, func(root string) string { return filepath.Join(root, "objects") }},
		{"bare probe refs", bareCloneRepo, func(root string) string { return filepath.Join(root, "refs") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const branch = "af-stat-stalled"
			root := tc.repo(t, branch)
			stallStatAt(t, tc.stall(root))
			useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)

			done := make(chan error, 1)
			start := time.Now()
			go func() { _, err := DeleteLocalBranch(root, branch); done <- err }()

			select {
			case err := <-done:
				require.Error(t, err,
					"a stalled stat must surface an error, never a silent deletion or a silent no-op")
				assert.Less(t, time.Since(start), 30*time.Second,
					"the stalled stat must be abandoned at its deadline, not waited on")
				assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut),
					"a stalled stat must classify as a branch-deletion timeout, as a stalled git does, got %v", err)
			case <-time.After(60 * time.Second):
				t.Fatal("DeleteLocalBranch HUNG on a stalled stat before any git ran: `af reset` " +
					"wedges with no completion time and no message")
			}

			// The branch survives: a stall is neither a deletion nor an absence.
			assert.True(t, refExists(t, root, branch),
				"a timed-out probe must not delete the branch it could not verify")

			// The read-only caller gets the same unknown, and a retry against the
			// same stuck path is refused by the shared latch at once instead of
			// paying the deadline again and stacking another blocked worker.
			exists, err := LocalBranchExists(root, branch)
			require.Error(t, err, "a stalled probe is UNKNOWN, never (false, nil)")
			assert.False(t, exists)
			assert.True(t, errors.Is(err, ErrBranchDeletionTimedOut), "got %v", err)
			assert.Contains(t, err.Error(), "still running after an earlier deadline",
				"a retry must join the timed-out latch, not start a second stat on the stuck path")
		})
	}
}

// BoundedStat keeps os.Stat's meaning: it follows a final symlink, which the
// branch-prune checks rely on (a repo root may be a symlink, as macOS /tmp is,
// and a bare layout is recognized by what its entries resolve to). It shares
// BoundedLstat's flight table, but a Stat and an Lstat of one path answer
// different questions, so a stalled Lstat must not refuse or answer a Stat.
func TestBoundedStatFollowsSymlinksAndIsKeyedApartFromLstat(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	info, err := BoundedStat(link)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "BoundedStat must follow the symlink to its directory")
	linfo, err := BoundedLstat(link)
	require.NoError(t, err)
	assert.NotZero(t, linfo.Mode()&os.ModeSymlink, "BoundedLstat must report the link itself")

	_, err = BoundedStat(filepath.Join(dir, "missing"))
	assert.True(t, os.IsNotExist(err), "ENOENT must come back as ENOENT, got %v", err)

	original := boundedLstatPath
	release := make(chan struct{})
	boundedLstatPath = func(p string) (os.FileInfo, error) {
		if p == link {
			<-release
		}
		return original(p)
	}
	t.Cleanup(func() { boundedLstatPath = original })
	t.Cleanup(func() { close(release) })
	useRelocationIdentityTimeoutForTest(t, 50*time.Millisecond)

	_, err = BoundedLstat(link)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)

	info, err = BoundedStat(link)
	require.NoError(t, err, "a timed-out Lstat of the same path must not latch the Stat")
	assert.True(t, info.IsDir())
}

// A bare layout whose objects directory is a symlink is still a bare layout:
// git follows it, and so must the probe. Read through Lstat, the symlink is not
// a directory, the root reads as "de-git'd", and the branch is reported absent
// while it verifiably exists, so the reset silently keeps it.
func TestDeleteLocalBranch_BareLayoutFollowsSymlinkedMetadata(t *testing.T) {
	const branch = "af-bare-linked"
	bare := bareCloneRepo(t, branch)
	moved := filepath.Join(t.TempDir(), "objects")
	require.NoError(t, os.Rename(filepath.Join(bare, "objects"), moved))
	require.NoError(t, os.Symlink(moved, filepath.Join(bare, "objects")))
	require.True(t, refExists(t, bare, branch), "sanity: git reads the symlinked layout")

	deleted, err := DeleteLocalBranch(bare, branch)
	require.NoError(t, err)
	assert.True(t, deleted, "a symlinked bare layout must be recognized and its branch deleted")
	assert.False(t, refExists(t, bare, branch))
}

package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrivateMoveResidueName pins the exact naming the cross-device relocate
// layer leaves beside its endpoints: `.af-source-<32hex>` and `.af-copy-<32hex>`.
// A mismatched purpose, a wrong-length suffix, or a non-hex suffix must not
// match, so a directory af did not write is never mistaken for stranded af
// residue.
func TestPrivateMoveResidueName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{".af-source-0123456789abcdef0123456789abcdef", true},
		{".af-copy-fedcba9876543210fedcba9876543210", true},
		{".af-source-abcdef", false},                                 // too short
		{".af-source-0123456789abcdef0123456789abcde", false},        // 31 chars
		{".af-source-0123456789abcdef0123456789abcdeg", false},       // non-hex
		{".af-source-0123456789abcdef0123456789abcdef-extra", false}, // trailing
		{".af-delete-0123456789abcdef0123456789abcdef", false},       // wrong purpose
		{".af-source-", false},                                       // empty suffix
		{".af-source", false},                                        // no separator
		{"af-source-0123456789abcdef0123456789abcdef", false},        // no leading dot
		{".af-copy-0123456789abcdef0123456789abcdef", true},
		{"worktree", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, privateMoveResidueName(c.name))
		})
	}
}

// TestListAFRelocationResidue finds the `.af-source-` quarantine and the
// `.af-copy-` staging copy af's cross-device move leaves behind, ignores the
// ordinary sibling the recorded path itself occupied, and treats an absent
// directory as no residue rather than an error.
func TestListAFRelocationResidue(t *testing.T) {
	parent := t.TempDir()
	other := t.TempDir()

	quarantine := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(quarantine, 0o755))
	staging := filepath.Join(other, ".af-copy-fedcba9876543210fedcba9876543210")
	require.NoError(t, os.Mkdir(staging, 0o755))
	// Siblings that are NOT af residue: a regular worktree directory, a file
	// with a residue-shaped name, and a partial/wrong-purpose private name.
	require.NoError(t, os.Mkdir(filepath.Join(parent, "wt-session"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(parent, ".af-source-11111111111111111111111111111111"), []byte("file"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(parent, ".af-delete-0123456789abcdef0123456789abcdef"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(parent, ".af-source-short"), 0o755))

	residue, err := ListAFRelocationResidue(parent, other)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{quarantine, staging}, residue)
}

func TestListAFRelocationResidue_AbsentDirIsNotAnError(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "does-not-exist")
	residue, err := ListAFRelocationResidue(absent)
	require.NoError(t, err)
	assert.Empty(t, residue)
}

func TestListAFRelocationResidue_UnreadableDirErrors(t *testing.T) {
	dir := t.TempDir()
	quarantine := filepath.Join(dir, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(quarantine, 0o755))
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if os.Geteuid() == 0 {
		t.Skip("permission test is meaningless as root")
	}
	residue, err := ListAFRelocationResidue(dir)
	require.Error(t, err)
	assert.Nil(t, residue)
	assert.Contains(t, err.Error(), dir)
}

// TestClaimRelocationSource_CrashWindowReturnsBareENOENT pins the
// record-free ambiguity the daemon-level residue arbitration closes. A
// cross-device archive move cut off between its quarantine rename and its
// publish rename renames the recorded worktree wholesale to a
// `.af-source-<32hex>` sibling and leaves a complete staged copy at a
// `.af-copy-<32hex>` sibling of the (still absent) destination, with no
// relocation record claiming either. ClaimRelocationSource probes only the
// recorded pathname and answers a PLAIN os.ErrNotExist — not joined with
// ErrRelocateStateUnknown — because from the git layer's perspective the
// recorded path is simply gone. That is exactly the shape that used to route
// daemon/archive.go into archiveSessionSourceAbsent and onto the "deleted
// outside af" branch before residue detection. The residue itself is
// discoverable with ListAFRelocationResidue, which is what the daemon now
// relies on.
func TestClaimRelocationSource_CrashWindowReturnsBareENOENT(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	destParent := t.TempDir()
	dest := filepath.Join(destParent, "archived")

	// Simulate the crash midpoint: the source rename to a private quarantine
	// sibling has happened, a complete copy has been staged beside the
	// destination, the publish rename has NOT, and no recovery record was
	// materialized.
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(wtPath, quarantine))
	copyTreeForTest(t, quarantine, dest)
	require.NoError(t, os.RemoveAll(dest))
	staging := filepath.Join(destParent, ".af-copy-"+strings.Repeat("b", 32))
	copyTreeForTest(t, quarantine, staging)

	// Reload the GitWorktree from storage so it carries no in-memory relocation
	// record from the original construction (a daemon restart sees this state).
	reloaded, err := NewGitWorktreeFromStorage(repoRoot, wtPath, "arch", "arch/branch", "", false, true)
	require.NoError(t, err)
	require.False(t, reloaded.hasRelocationRecoveryRecord(),
		"a crash between the quarantine and publish renames leaves no relocation record; recovery is record-free")

	_, claimErr := reloaded.ClaimRelocationSource()
	require.Error(t, claimErr)
	assert.True(t, errors.Is(claimErr, os.ErrNotExist),
		"the recorded path is absent, so the claim must surface ENOENT: %v", claimErr)
	assert.False(t, errors.Is(claimErr, ErrRelocateStateUnknown),
		"a record-free ENOENT must NOT be joined with ErrRelocateStateUnknown; that is what routes the daemon onto the residue arbitration instead of the refusal-persist path: %v", claimErr)

	// The quarantined worktree — the only copy of the uncommitted work — is
	// still on disk and untouched, including the dirty file, exactly the loss
	// the residue arbitration exists to prevent.
	dirty, err := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uncommitted work", string(dirty))

	// The residue is discoverable beside both endpoints.
	residue, resErr := ListAFRelocationResidue(filepath.Dir(wtPath), destParent)
	require.NoError(t, resErr)
	assert.ElementsMatch(t, []string{quarantine, staging}, residue)
}

// copyTreeForTest copies src into dst (a faithful stand-in for the
// cross-device relocate layer's copyTreeWithPolicy, which is unexported).
func copyTreeForTest(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o755))
	require.NoError(t, walkCopy(src, dst))
}

func walkCopy(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			if err := os.MkdirAll(d, info.Mode()); err != nil {
				return err
			}
			if err := walkCopy(s, d); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(s)
			if lerr != nil {
				return lerr
			}
			if err := os.Symlink(target, d); err != nil {
				return err
			}
		default:
			data, rerr := os.ReadFile(s)
			if rerr != nil {
				return rerr
			}
			if err := os.WriteFile(d, data, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestRelocateCrashWindow_RealCrossDeviceMoveLeavesResidue is the faithful
// end-to-end reproduction of the bug's crash window. Unlike the
// single-process tests above, which simulate the crash footprint by renaming
// the source with os.Rename, this test drives the REAL
// moveDirCrossDeviceRecordingIdentity machinery across a REAL second
// filesystem (loopback ext4, kernel-reported EXDEV) and models the daemon
// death by os.Exit(9) inside the moveDirBeforeDestCommit seam — which fires
// AFTER the quarantine rename has committed the source to
// `.af-source-<32hex>` and a complete copy has been staged at
// `.af-copy-<32hex>`, but BEFORE the publish rename. os.Exit prevents the
// deferred staging-cleanup and source-restore from running, exactly as a
// SIGKILL/OOM would. The parent process then assertsends the on-disk
// footprint matches the bug report (source absent, dest absent, complete
// worktree stranded under `.af-source-*` with the uncommitted file, staged
// copy under `.af-copy-*`) and that ListAFRelocationResidue discovers both.
//
// Precondition: a second writable filesystem. The test attempts to use
// /dev/vdb-style loopback: it searches common mount points for a directory
// whose st_dev differs from /tmp's; if none exists, it creates a loopback
// ext4 image and mounts it (requires CAP_SYS_ADMIN; skipped if unavailable).
func TestRelocateCrashWindow_RealCrossDeviceMoveLeavesResidue(t *testing.T) {
	if os.Getenv("AF_CRASH_CHILD_SECOND_FS") == "1" {
		runChildRelocateCrash(t)
		return
	}

	secondFS := findOrMakeSecondFilesystem(t)
	srcParent := t.TempDir()
	destParent := secondFS

	src := filepath.Join(srcParent, "wt-crash")
	require.NoError(t, os.Mkdir(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "dirty.txt"), []byte("uncommitted work"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "untracked.txt"), []byte("untracked work"), 0o644))
	dest := filepath.Join(destParent, "archived-crash")

	// Drive the REAL cross-device move in a child process so an os.Exit mid
	// move does not take the test process with it. The child re-execs this
	// test binary with a -test.run filter that selects the child harness and
	// env pointing at the already-prepared src/dest and second-fs marker.
	cmd := exec.Command(os.Args[0], "-test.run=^TestRelocateCrashWindow_RealCrossDeviceMoveLeavesResidue$", "-test.timeout=60s")
	cmd.Dir = srcParent
	cmd.Env = append(os.Environ(),
		"AF_CRASH_CHILD_SECOND_FS=1",
		"AF_CRASH_SRC="+src,
		"AF_CRASH_DEST="+dest,
	)
	var childOut strings.Builder
	cmd.Stdout = &childOut
	cmd.Stderr = &childOut
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	require.True(t, ok, "the child must self-terminate via os.Exit(9) to model the crash; got %v\noutput: %s", err, childOut.String())
	assert.Equal(t, 9, exitErr.ExitCode(),
		"the child must have exited via the os.Exit(9) seam, not completed the move")

	// Source is gone (the quarantine rename took it) and dest was never
	// published.
	_, statErr := os.Lstat(src)
	assert.True(t, os.IsNotExist(statErr),
		"the recorded source path must be absent after the quarantine rename: %v", statErr)
	assert.NoDirExists(t, dest, "the publish rename never ran, so dest must be absent")

	// The complete worktree is stranded under `.af-source-*` beside the
	// vacated source, with the uncommitted AND untracked files intact.
	sourceResidue, sErr := ListAFRelocationResidue(filepath.Dir(src))
	require.NoError(t, sErr)
	require.Len(t, sourceResidue, 1, "exactly one .af-source-* quarantine must remain beside the source")
	quarantine := sourceResidue[0]
	assert.True(t, strings.HasPrefix(filepath.Base(quarantine), ".af-source-"))
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr, "the stranded worktree's uncommitted file must survive")
	assert.Equal(t, "uncommitted work", string(dirty))
	untracked, uerr := os.ReadFile(filepath.Join(quarantine, "untracked.txt"))
	require.NoError(t, uerr, "untracked files in the stranded worktree must survive too")
	assert.Equal(t, "untracked work", string(untracked))

	// A complete staged copy sits at `.af-copy-*` beside the (absent) dest.
	destResidue, dErr := ListAFRelocationResidue(filepath.Dir(dest))
	require.NoError(t, dErr)
	require.Len(t, destResidue, 1, "exactly one .af-copy-* staging must remain beside the destination")
	staging := destResidue[0]
	assert.True(t, strings.HasPrefix(filepath.Base(staging), ".af-copy-"))
	stagedDirty, sdErr := os.ReadFile(filepath.Join(staging, "dirty.txt"))
	require.NoError(t, sdErr, "the staged copy must carry the uncommitted file")
	assert.Equal(t, "uncommitted work", string(stagedDirty))

	// ListAFRelocationResidue over both parents returns exactly both paths.
	both, bErr := ListAFRelocationResidue(filepath.Dir(src), filepath.Dir(dest))
	require.NoError(t, bErr)
	assert.ElementsMatch(t, []string{quarantine, staging}, both)
}

// runChildRelocateCrash is the child harness: it sets the EXDEV seam (a real
// second filesystem is already in use, but the seam guarantees the fallback
// path is taken even if the kernel would not return EXDEV), installs the
// crash seam at moveDirBeforeDestCommit, and runs the real
// moveDirCrossDevice. The os.Exit(9) inside the seam models the daemon
// death: it fires AFTER the quarantine rename and staged copy, BEFORE the
// publish, and it prevents the deferred cleanup from restoring the source
// or removing the staging — exactly the footprint a SIGKILL/OOM leaves.
func runChildRelocateCrash(t *testing.T) {
	src := os.Getenv("AF_CRASH_SRC")
	dest := os.Getenv("AF_CRASH_DEST")
	if src == "" || dest == "" {
		t.Fatal("AF_CRASH_SRC/AF_CRASH_DEST not set")
	}
	moveDirBeforeDestCommit = func(string) error {
		os.Exit(9)
		return nil
	}
	_ = moveDirCrossDevice(src, dest, "move")
	t.Fatalf("the moveDirBeforeDestCommit seam did not fire; move completed unexpectedly")
}

// findOrMakeSecondFilesystem locates a directory whose st_dev differs from
// /tmp's so the kernel returns a real EXDEV for a cross-dir rename. If none
// of the common mount points qualifies, it creates a loopback ext4 image and
// mounts it. The test is skipped if neither is possible.
func findOrMakeSecondFilesystem(t *testing.T) string {
	t.Helper()
	tmpStat, err := os.Stat("/tmp")
	require.NoError(t, err)
	tmpDev := statDev(tmpStat)
	for _, cand := range []string{"/dev/shm", "/run", "/var/tmp"} {
		if info, err := os.Stat(cand); err == nil && statDev(info) != tmpDev {
			dir := filepath.Join(cand, "af-crash-second-fs-"+t.Name())
			require.NoError(t, os.MkdirAll(dir, 0o755))
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	// Try a loopback mount: create an ext4 image and mount it. Requires
	// CAP_SYS_ADMIN; skip if unavailable.
	img := filepath.Join(t.TempDir(), "second.img")
	require.NoError(t, exec.Command("truncate", "-s", "32m", img).Run())
	if err := exec.Command("mkfs.ext4", "-q", img).Run(); err != nil {
		t.Skipf("no second filesystem available and mkfs.ext4 failed (%v); cannot run real cross-device crash test", err)
	}
	mnt := filepath.Join(t.TempDir(), "mnt")
	require.NoError(t, os.MkdirAll(mnt, 0o755))
	if err := exec.Command("mount", "-o", "loop", img, mnt).Run(); err != nil {
		t.Skipf("no second filesystem available and loopback mount failed (%v); cannot run real cross-device crash test", err)
	}
	t.Cleanup(func() { _ = exec.Command("umount", mnt).Run() })
	return mnt
}

func statDev(info os.FileInfo) uint64 {
	type devGetter interface{ Dev() uint64 }
	if d, ok := info.Sys().(devGetter); ok {
		return d.Dev()
	}
	return 0
}

// ensure the time import is used when the child-harness path is compiled out
var _ = time.Second

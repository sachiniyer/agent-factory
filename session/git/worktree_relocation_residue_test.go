package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

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

// unknownTypeDirEntry wraps a real os.DirEntry but reports DT_UNKNOWN
// (Type() == 0) the way some NFS/FUSE filesystems do from readdir, so the
// residue scan cannot trust the entry type and must stat it.
type unknownTypeDirEntry struct {
	os.DirEntry
}

func (unknownTypeDirEntry) Type() os.FileMode { return 0 }
func (unknownTypeDirEntry) IsDir() bool       { return false }

// TestListAFRelocationResidue_DTUnknownDirIsStatChecked pins the fail-closed
// handling of DT_UNKNOWN entries: when readdir does not fill in the type,
// IsDir() returns false, so a real `.af-source-*`/`.af-copy-*` directory would
// be skipped and the caller could archive the session as a deletion despite
// the stranded work. The scan must stat the entry instead, find the
// directory, and report the residue.
func TestListAFRelocationResidue_DTUnknownDirIsStatChecked(t *testing.T) {
	parent := t.TempDir()
	quarantine := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(quarantine, 0o755))
	// A residue-shaped regular file must still be skipped even when its
	// readdir type is unknown: the stat says it is not a directory.
	require.NoError(t, os.WriteFile(filepath.Join(parent, ".af-copy-fedcba9876543210fedcba9876543210"), []byte("file"), 0o644))

	restore := SetArchiveReadDirForTest(parent, func(observed string) ([]os.DirEntry, error) {
		real, err := os.ReadDir(observed)
		if err != nil {
			return nil, err
		}
		out := make([]os.DirEntry, len(real))
		for i, e := range real {
			out[i] = unknownTypeDirEntry{DirEntry: e}
		}
		return out, nil
	})
	defer restore()

	residue, err := ListAFRelocationResidue(parent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{quarantine}, residue,
		"a DT_UNKNOWN residue directory must be stat-checked and found; a DT_UNKNOWN residue-shaped file must still be skipped")
}

// TestListAFRelocationResidue_DTUnknownInspectErrorPropagates pins the
// fail-closed contract: when the entry type is unknown and the stat to resolve
// it fails, the scan returns an error rather than silently skipping the entry
// and risking a deletion verdict over a scan it could not complete. The stat
// now runs through BoundedLstat on the joined path (the reviewer's bound), so
// the failure is modeled by overriding the bounded lstat seam to error on the
// candidate, not by the entry's own (unused) Info().
func TestListAFRelocationResidue_DTUnknownInspectErrorPropagates(t *testing.T) {
	parent := t.TempDir()
	const name = ".af-source-0123456789abcdef0123456789abcdef"
	candidate := filepath.Join(parent, name)
	// A residue-shaped DirEntry whose readdir type is DT_UNKNOWN, modeling an
	// NFS/FUSE filesystem that did not fill in d_type. The scan must stat the
	// joined path to resolve it.
	var entry os.DirEntry = unknownTypeNamedEntry{name: name}

	restoreRead := SetArchiveReadDirForTest(parent, func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{entry}, nil
	})
	defer restoreRead()

	original := boundedLstatPath
	boundedLstatPath = func(p string) (os.FileInfo, error) {
		if p == candidate {
			return nil, os.ErrPermission
		}
		return original(p)
	}
	t.Cleanup(func() { boundedLstatPath = original })

	_, err := ListAFRelocationResidue(parent)
	require.Error(t, err, "an unresolvable entry type must fail-closed, not be skipped")
}

// unknownTypeNamedEntry is a DirEntry with a caller-chosen name and an unknown
// readdir type (DT_UNKNOWN), modeling NFS/FUSE filesystems that do not fill in
// d_type. IsDir() returns false (as it does when the type is unknown) so the
// scan must stat the joined path to resolve it.
type unknownTypeNamedEntry struct {
	name string
}

func (e unknownTypeNamedEntry) Name() string               { return e.name }
func (unknownTypeNamedEntry) IsDir() bool                  { return false }
func (unknownTypeNamedEntry) Type() os.FileMode            { return 0 }
func (e unknownTypeNamedEntry) Info() (os.FileInfo, error) { return os.Lstat(e.name) }

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
// The cross-device path is forced by an EXDEV answer at the first rename, so
// every write stays inside t.TempDir() — the engine's EXDEV branch is
// identical either way, EXDEV is only what selects it.
func TestRelocateCrashWindow_RealCrossDeviceMoveLeavesResidue(t *testing.T) {
	if os.Getenv("AF_CRASH_CHILD_SECOND_FS") == "1" {
		runChildRelocateCrash(t)
		return
	}

	srcParent := t.TempDir()
	destParent := t.TempDir()

	src := filepath.Join(srcParent, "wt-crash")
	require.NoError(t, os.Mkdir(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "dirty.txt"), []byte("uncommitted work"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "untracked.txt"), []byte("untracked work"), 0o644))
	dest := filepath.Join(destParent, "archived-crash")

	// Drive the REAL cross-device move in a child process so an os.Exit mid
	// move does not take the test process with it. The child re-execs this
	// test binary with a -test.run filter that selects the child harness and
	// env pointing at the already-prepared src/dest and second-fs marker.
	// The child's os.Exit(9) skips every deferred cleanup INCLUDING the
	// one that removes the sandbox home TestMain stamps under TMPDIR —
	// each crashed child would leak one. Point the child's temp root at
	// a directory the parent owns so whatever it leaks dies with this
	// test's tree.
	childTmp := filepath.Join(t.TempDir(), "child-tmp")
	require.NoError(t, os.Mkdir(childTmp, 0o755))
	cmd := exec.Command(os.Args[0], "-test.run=^TestRelocateCrashWindow_RealCrossDeviceMoveLeavesResidue$", "-test.timeout=60s")
	cmd.Dir = srcParent
	cmd.Env = append(os.Environ(),
		"AF_CRASH_CHILD_SECOND_FS=1",
		"AF_CRASH_SRC="+src,
		"AF_CRASH_DEST="+dest,
		"TMPDIR="+childTmp,
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

// runChildRelocateCrash is the child harness: it installs the EXDEV answer
// for the engine's first same-filesystem rename (so the cross-device path
// runs with every write inside the test's temp dirs — no second filesystem,
// no loopback mount, no /dev/shm, /run or /var/tmp writes), installs the
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
	renamePath = func(string, string) error { return syscall.EXDEV }
	moveDirBeforeDestCommit = func(string) error {
		os.Exit(9)
		return nil
	}
	_ = moveDirCrossDevice(src, dest, "move")
	t.Fatalf("the moveDirBeforeDestCommit seam did not fire; move completed unexpectedly")
}

// TestBindAFRelocationResidue_OwnsQuarantine pins the proof that a residue
// directory is this session's own stranded worktree: af's cross-device move is
// a raw rename, so git's registration keeps backpointing at the recorded (now
// absent) source path, and the residue's `.git` pointer resolves into this
// repo's metadata. Such a residue blocks this session's archive.
func TestBindAFRelocationResidue_OwnsQuarantine(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(wtPath, quarantine))

	binding, err := BindAFRelocationResidue(quarantine, wtPath, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, ResidueOurs, binding, "the quarantined worktree backpoints at this session's recorded source")
}

// TestBindAFRelocationResidue_OwnsStagingCopy pins that the staged `.af-copy-*`
// directory — a copy of the source — shares the source's git binding, so it too
// is recognized as this session's residue and not mistaken for foreign.
func TestBindAFRelocationResidue_OwnsStagingCopy(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	quarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(wtPath, quarantine))
	stagingParent := t.TempDir()
	staging := filepath.Join(stagingParent, ".af-copy-"+strings.Repeat("b", 32))
	copyTreeForTest(t, quarantine, staging)

	binding, err := BindAFRelocationResidue(staging, wtPath, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, ResidueOurs, binding, "the staged copy shares the source's git binding")
}

// TestBindAFRelocationResidue_ForeignResidueIsNotOurs is the core of the
// shared-parent concern: a residue directory belonging to ANOTHER session
// (another session's interrupted move, or a tree another session's archive
// retained on purpose) sits beside this session's source parent and must NOT
// block this session's archive. Its `.git` pointer resolves into this repo but
// its registration backpointer names the OTHER session's recorded path, so the
// binding is foreign.
func TestBindAFRelocationResidue_ForeignResidueIsNotOurs(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	// A second, independent worktree in the SAME repo (another session).
	otherBranch := "arch/other"
	otherWt := filepath.Join(filepath.Dir(repoRoot), "repo-arch-other")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", otherBranch, otherWt)
	// Strand the other session's worktree under a residue name beside this
	// session's source parent, modelling another session's interrupted move.
	foreign := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("c", 32))
	require.NoError(t, os.Rename(otherWt, foreign))

	binding, err := BindAFRelocationResidue(foreign, wtPath, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, ResidueForeign, binding,
		"a residue whose registration backpoints at another session's path is foreign and must not block this archive")
}

// TestBindAFRelocationResidue_ForeignRepositoryResidueIsNotOurs pins that a
// residue pointing into a DIFFERENT repository's metadata is foreign, so a
// foreign repo's worktree parked beside this session's source parent does not
// block this archive.
func TestBindAFRelocationResidue_ForeignRepositoryResidueIsNotOurs(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	// An unrelated repo with its own worktree.
	otherRepo := createGitRepo(t)
	runGitInPlaceTest(t, otherRepo, "commit", "--allow-empty", "-m", "init")
	otherWt := filepath.Join(filepath.Dir(otherRepo), "other-repo-arch")
	runGitInPlaceTest(t, otherRepo, "worktree", "add", "-b", "arch/other", otherWt)
	foreign := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("d", 32))
	require.NoError(t, os.Rename(otherWt, foreign))

	binding, err := BindAFRelocationResidue(foreign, wtPath, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, ResidueForeign, binding,
		"a residue whose .git pointer resolves into a different repository is foreign")
}

// TestBindAFRelocationResidue_UnverifiedNoGitPointerIsUnknown pins the
// fail-closed shape: a residue-shaped directory that carries no linked-worktree
// `.git` pointer (e.g. a stale orphan a completed move left, or a directory a
// user created with af's private naming) cannot be proven foreign or ours. The
// binding is unknown; the caller refuses but must not claim it is this
// session's work.
func TestBindAFRelocationResidue_UnverifiedNoGitPointerIsUnknown(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	orphan := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("e", 32))
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))

	binding, err := BindAFRelocationResidue(orphan, wtPath, repoRoot)
	require.Error(t, err, "a residue with no readable git binding is unverified, not silently classified")
	assert.Equal(t, ResidueUnknown, binding)
}

// TestBindAFRelocationResidue_SymlinkCandidateIsUnverified pins R6: the bind
// must not traverse the candidate by pathname. A symlink planted at the
// residue path — swapped in after the scan saw a directory — must fail the
// O_NOFOLLOW open and read as unverified, never as the tree the link resolves
// into. On master the pathname traversal follows the link and classifies the
// pointed-at foreign tree as foreign, so a planted link can launder a
// refusal into "not mine, proceed".
func TestBindAFRelocationResidue_SymlinkCandidateIsUnverified(t *testing.T) {
	_, repoRoot, wtPath := archiveTestWorktree(t)
	otherBranch := "arch/other"
	otherWt := filepath.Join(filepath.Dir(repoRoot), "repo-arch-other")
	runGitInPlaceTest(t, repoRoot, "worktree", "add", "-b", otherBranch, otherWt)
	candidate := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("f", 32))
	require.NoError(t, os.Symlink(otherWt, candidate))

	binding, err := BindAFRelocationResidue(candidate, wtPath, repoRoot)
	require.Error(t, err, "a symlink at the candidate path is not a scanned directory; the bind must not follow it")
	assert.Equal(t, ResidueUnknown, binding)
}

// TestListAFRelocationResidue_DanglingSymlinkParentRefuses pins R7: when the
// directory the scan was asked to enumerate resolves through a symlink whose
// target does not exist, readdir's ENOENT is not "absent" — the link could be
// fixed to resolve somewhere carrying residue, so "no residue" is unproven
// and the scan must refuse rather than report an empty parent.
func TestListAFRelocationResidue_DanglingSymlinkParentRefuses(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "worktrees")
	require.NoError(t, os.Symlink(filepath.Join(root, "nonexistent"), link))

	residue, err := ListAFRelocationResidue(link)
	require.Error(t, err, "a dangling symlink in the scanned path cannot prove the directory carries no residue")
	assert.Nil(t, residue)
	assert.Contains(t, err.Error(), link)
}

// TestListAFRelocationResidue_DanglingSymlinkInParentChainRefuses pins the
// same refusal when the dangling component sits deeper inside the scanned
// path, not at its leaf.
func TestListAFRelocationResidue_DanglingSymlinkInParentChainRefuses(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(filepath.Join(root, "nonexistent"), link))

	residue, err := ListAFRelocationResidue(filepath.Join(link, "sub"))
	require.Error(t, err)
	assert.Nil(t, residue)
	assert.Contains(t, err.Error(), link)
}

// TestListAFRelocationResidue_ResolvedSymlinkParentStillScans pins that only a
// DANGLING link refuses: a parent path that traverses a symlink resolving to
// a real directory is scanned normally and its residue is found.
func TestListAFRelocationResidue_ResolvedSymlinkParentStillScans(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	quarantine := filepath.Join(real, ".af-source-"+strings.Repeat("1", 32))
	require.NoError(t, os.Mkdir(quarantine, 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	residue, err := ListAFRelocationResidue(link)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{filepath.Join(link, ".af-source-"+strings.Repeat("1", 32))}, residue)
}

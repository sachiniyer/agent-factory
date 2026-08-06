package git

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// This file holds the differential guard for moveDirCrossDevice's two
// implementations of one promise (#2919).
//
// Same-device is rename(2), which carries every filesystem property for free.
// Cross-device is a byte copy that reproduces only what someone wrote code for.
// The defect that produced #2869, #2872 and #2920 was never really any one of
// those properties — it was that the copier's fidelity is an ENUMERATION, and
// nothing failed when the enumeration was incomplete. Each gap was found by a
// user or an audit, one at a time.
//
// So this test does not check a list of properties someone remembered. It runs
// one fixture through BOTH paths, compares everything it can observe, and fails
// on any difference that is not in the inventory below. A newly-diverging
// property fails here rather than in someone's archive.

// knownCrossDeviceDivergence is the inventory of properties the copy is known
// NOT to reproduce, each tracked in #2919. It is deliberately a denylist rather
// than the assertion being an allowlist: an allowlist silently ignores anything
// nobody thought of, which is the exact failure this test exists to prevent.
//
// The test fails in BOTH directions on purpose. Add a property to the copier
// without removing it here and the "no longer diverges" branch fires, so the
// inventory cannot rot into a list of things that were fixed years ago.
var knownCrossDeviceDivergence = map[string]string{
	"mtime.symlink":          "#2919: the link's own mtime is not set — there is no portable dirfd-relative lstat to read it from, and the TARGET's mtime is what tools actually read",
	"hardlink.externalNlink": "#2919: an inode with one name inside the worktree and another OUTSIDE it keeps both aliases across a rename, but the copy can only create the in-tree name — there is nothing it could link the outside name to. Unfixable by construction, unlike the in-tree pair.",
	"xattr.file":             "#2919: no xattr namespace is copied, so ACLs, capabilities and SELinux labels are dropped",
	"xattr.dir":              "#2919: same cause, on directories",
}

// TestMoveDirCrossDevice_CopyDivergesFromRenameOnlyWhereRecorded is the class
// guard for #2919. Everything not in the inventory above must be identical
// through both paths.
func TestMoveDirCrossDevice_CopyDivergesFromRenameOnlyWhereRecorded(t *testing.T) {
	// A restrictive umask is load-bearing, not incidental. Under the ambient
	// 0022 the fixture's modes survive mkdirat/openat untouched, so breaking
	// mode preservation entirely produced no observable difference and this
	// guard passed — the exact blind spot that let #2869 ship green. Measured.
	withUmask(t, 0077)
	workspace := t.TempDir()

	renamedSource := filepath.Join(workspace, "renamed-src")
	writeFidelityProbeTree(t, renamedSource)
	before := describeFidelity(t, renamedSource)
	renamedDest := filepath.Join(workspace, "renamed-dest")
	require.NoError(t, moveDirCrossDevice(renamedSource, renamedDest),
		"same-device move must take the rename fast path")
	afterRename := describeFidelity(t, renamedDest)

	copiedSource := filepath.Join(workspace, "copied-src")
	writeFidelityProbeTree(t, copiedSource)
	originalRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = originalRename })
	copiedDest := filepath.Join(workspace, "copied-dest")
	require.NoError(t, moveDirCrossDevice(copiedSource, copiedDest))
	afterCopy := describeFidelity(t, copiedDest)

	// The rename leg is the oracle, and this is what makes it one: if rename
	// ever stopped carrying a property, comparing the copy against it would
	// quietly agree on the wrong answer.
	for property, want := range before {
		assert.Equal(t, want, afterRename[property],
			"rename(2) is this test's oracle and it must be lossless, but %s changed", property)
	}

	for property, sourceValue := range before {
		copyValue := afterCopy[property]
		// The one legitimate reason to skip: the fixture could not set the
		// property in the first place, so neither tree can say anything about
		// it. Decided from the SOURCE description only — asking the copy would
		// let a dropped property masquerade as an unsupported filesystem.
		if sourceValue == "MISSING" {
			t.Logf("skipping %s: this filesystem could not set it on the fixture", property)
			continue
		}
		reason, recorded := knownCrossDeviceDivergence[property]
		switch {
		case copyValue == sourceValue && recorded:
			t.Errorf("%s no longer diverges (source=%s copy=%s) — remove it from "+
				"knownCrossDeviceDivergence so the inventory keeps meaning what it says (%s)",
				property, sourceValue, copyValue, reason)
		case copyValue != sourceValue && !recorded:
			t.Errorf("%s diverges between the two move paths and is NOT a recorded gap: "+
				"source=%s rename=%s copy=%s. An archive is supposed to be restorable and "+
				"identical — either preserve it in the copier, or record it in "+
				"knownCrossDeviceDivergence with an issue reference",
				property, sourceValue, afterRename[property], copyValue)
		}
	}
}

// writeFidelityProbeTree builds a fixture that exercises the filesystem
// properties an archive can lose. Anything the local filesystem cannot express
// is simply left out of the description rather than skipped silently — see
// describeFidelity.
func writeFidelityProbeTree(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dir"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty-dir"), 0700))

	require.NoError(t, os.WriteFile(filepath.Join(root, "hard-a"), []byte("shared payload"), 0600))
	require.NoError(t, os.Link(filepath.Join(root, "hard-a"), filepath.Join(root, "hard-b")))

	// A second link group whose other alias lives OUTSIDE the tree being moved.
	// Both aliases of hard-a/hard-b travel together, so the copy can reproduce
	// that group; this one it structurally cannot, because the outside name is
	// not its to create. Without this shape the guard would claim parity for a
	// valid source layout that still diverges.
	require.NoError(t, os.WriteFile(filepath.Join(root, "ext-linked.txt"), []byte("aliased outside"), 0600))
	require.NoError(t, os.Link(
		filepath.Join(root, "ext-linked.txt"),
		filepath.Join(filepath.Dir(root), "ext-alias-"+filepath.Base(root)),
	))

	require.NoError(t, os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain"), 0600))
	require.NoError(t, os.Symlink("plain.txt", filepath.Join(root, "link")))

	sparse, err := os.Create(filepath.Join(root, "sparse.bin"))
	require.NoError(t, err)
	require.NoError(t, sparse.Truncate(8<<20))
	_, err = sparse.WriteAt([]byte("payload"), 4<<20)
	require.NoError(t, err)
	require.NoError(t, sparse.Close())

	// Best effort: a filesystem without user xattrs just omits the property.
	_ = unix.Setxattr(filepath.Join(root, "plain.txt"), "user.af_fidelity", []byte("filevalue"), 0)
	_ = unix.Setxattr(filepath.Join(root, "dir"), "user.af_fidelity", []byte("dirvalue"), 0)

	// Explicitly, and after every node exists: os.Mkdir/os.WriteFile subtract
	// the umask themselves, so a fixture that is merely REQUESTED at these modes
	// is born already tightened and proves nothing.
	for path, mode := range map[string]os.FileMode{
		filepath.Join(root, "dir"):       0755,
		filepath.Join(root, "empty-dir"): 0750,
		filepath.Join(root, "plain.txt"): 0640,
		filepath.Join(root, "hard-a"):    0664,
	} {
		require.NoError(t, os.Chmod(path, mode))
	}

	old := time.Unix(1_000_000_000, 0)
	require.NoError(t, os.Chtimes(filepath.Join(root, "plain.txt"), old, old))
	require.NoError(t, os.Chtimes(filepath.Join(root, "dir"), old, old))
	// The link too, via AT_SYMLINK_NOFOLLOW — os.Chtimes follows it and would
	// stamp plain.txt a second time, leaving the link itself reading "now".
	// Without this the mtime.symlink probe would still show a divergence, but
	// only because the two fixtures are built at different moments; stamping it
	// means the probe fails for the reason it claims to.
	stamp := unix.NsecToTimespec(old.UnixNano())
	require.NoError(t, unix.UtimesNanoAt(
		unix.AT_FDCWD, filepath.Join(root, "link"), []unix.Timespec{stamp, stamp}, unix.AT_SYMLINK_NOFOLLOW,
	))
}

// describeFidelity renders one tree as property → observed value. A property the
// local filesystem cannot express is omitted entirely, so the comparison above
// skips it instead of inventing a difference (or, worse, an agreement).
func describeFidelity(t *testing.T, root string) map[string]string {
	t.Helper()
	described := map[string]string{}

	var hardA syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(root, "hard-a"), &hardA))
	described["hardlink.nlink"] = fmt.Sprintf("%d", hardA.Nlink)
	var hardB syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(root, "hard-b"), &hardB))
	described["hardlink.sameInode"] = fmt.Sprintf("%t", hardA.Ino == hardB.Ino)

	var external syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(root, "ext-linked.txt"), &external))
	described["hardlink.externalNlink"] = fmt.Sprintf("%d", external.Nlink)

	var sparseStat syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(root, "sparse.bin"), &sparseStat))
	described["sparse.size"] = fmt.Sprintf("%d", sparseStat.Size)
	// Bucketed, not exact: cp --sparse=always reproduces holes at write
	// granularity, so demanding an identical block count would fail on a
	// faithful copy. What must not happen is the hole being allocated.
	described["sparse.mostlyUnallocated"] = fmt.Sprintf("%t", sparseStat.Blocks*512 < sparseStat.Size/2)

	// Always recorded, never omitted. An earlier version of this decided here
	// whether the filesystem supported xattrs by re-probing the tree it was
	// describing — which cannot tell "this filesystem has no xattrs" from "the
	// copy dropped them", so the property was silently left out of BOTH
	// descriptions and the comparison skipped it entirely. The capability
	// question belongs to the source description alone, and the caller asks it.
	for property, path := range map[string]string{
		"xattr.file": filepath.Join(root, "plain.txt"),
		"xattr.dir":  filepath.Join(root, "dir"),
	} {
		buffer := make([]byte, 64)
		if n, err := unix.Getxattr(path, "user.af_fidelity", buffer); err == nil {
			described[property] = string(buffer[:n])
		} else {
			described[property] = "MISSING"
		}
	}

	for property, path := range map[string]string{
		"mtime.file": filepath.Join(root, "plain.txt"),
		"mtime.dir":  filepath.Join(root, "dir"),
		// The link's OWN mtime, via Lstat. Measured even though it is not
		// preserved: an unmeasured gap is exactly how this class kept
		// recurring — a property nobody listed could not fail anything.
		"mtime.symlink": filepath.Join(root, "link"),
	} {
		info, err := os.Lstat(path)
		require.NoError(t, err)
		described[property] = info.ModTime().UTC().Format(time.RFC3339Nano)
	}

	for property, path := range map[string]string{
		"mode.dir":   filepath.Join(root, "dir"),
		"mode.file":  filepath.Join(root, "plain.txt"),
		"mode.empty": filepath.Join(root, "empty-dir"),
		"mode.hardA": filepath.Join(root, "hard-a"),
	} {
		info, err := os.Lstat(path)
		require.NoError(t, err)
		described[property] = fmt.Sprintf("%04o", info.Mode().Perm())
	}

	target, err := os.Readlink(filepath.Join(root, "link"))
	require.NoError(t, err)
	described["symlink.target"] = target

	contents, err := os.ReadFile(filepath.Join(root, "hard-a"))
	require.NoError(t, err)
	described["contents.hardA"] = string(contents)

	return described
}

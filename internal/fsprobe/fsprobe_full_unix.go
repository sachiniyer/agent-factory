//go:build unix

package fsprobe

import "syscall"

// statfsFn is the test seam under Full — the metadata syscall that answers a
// filesystem's free-block count without needing write access to the path.
var statfsFn = syscall.Statfs

// fsFull runs statfs and reports whether the filesystem hosting dir is full —
// the fullness verdict a write probe cannot produce on a path the process
// cannot write to at all (a root-owned daemon data root).
//
// The block metric depends on WHO would write: a consumer that can allocate
// reserved blocks (a rootful dockerd — unprivileged false) fills the
// filesystem at Bfree == 0, while one writing as an ordinary user (a rootless
// dockerd — unprivileged true) gets ENOSPC the moment Bavail hits zero, while
// root-reserved blocks remain (#5382). Picking Bfree for a rootless engine is
// a false not-full during a real ENOSPC outage; picking Bavail for a rootful
// one is a false full while dockerd can still write — the caller must know
// which consumer it asks for.
//
// Inodes are checked alongside blocks: an exhausted inode table fails
// metadata creation with the same ENOSPC. The verdict reads Ffree only when
// the filesystem reports real inode accounting (Files > 0) — a filesystem
// that reports zero total inodes is not tracking them and cannot be out of
// them, so that combination is not-proven rather than full (#5382).
//
// btrfs is the exception to the Bfree rule for privileged writers: the
// space Bfree still counts once Bavail hits zero is an internal reserve no
// uid can allocate — root holds no extra room there, and ENOSPC arrives at
// Bavail == 0 while Bfree stays positive (#5382 play-test). ext4 is the
// counterexample: its reserve IS the root-usable pool, so a rootful dockerd
// still writes at Bavail == 0 and only Bfree == 0 is full. A filesystem
// whose reserve semantics are unproven keeps the Bfree rule — an unknown
// fstype must not invent a full verdict.
const btrfsFSType = 0x9123683e

func fsFull(dir string, unprivileged bool) (bool, error) {
	var st syscall.Statfs_t
	if err := statfsFn(dir, &st); err != nil {
		return false, err
	}
	freeBlocks := st.Bfree
	if unprivileged || st.Type == btrfsFSType {
		freeBlocks = st.Bavail
	}
	inodesExhausted := st.Files > 0 && st.Ffree == 0
	return freeBlocks == 0 || inodesExhausted, nil
}

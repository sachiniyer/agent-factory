package proctree

import "golang.org/x/sys/unix"

// openedDirMatchesVnode is the TOCTOU check openWorkingDir signals on: it
// reports whether the directory just opened by path is the same vnode the
// kernel reported as pid's cwd via PROC_PIDVNODEPATHINFO. A mismatch means the
// path was replaced between the proc_info read and the open(2) — a symlink
// shuffle an attacker could use to point a destructive reap at the wrong
// directory — and openWorkingDir must refuse it.
//
// It is split out of openWorkingDir (proctree_darwin.go) and kept free of a build
// tag for the same reason cwdIdentityFromVnodePathInfo is (vnodepathinfo.go): the
// check is pure arithmetic on fixed-width integers, so Linux CI exercises it on
// every run, while the darwin-only fstat that produces the inputs stays tagged.
// The decode half this is paired against lives in vnodepathinfo.go.
//
// statDev carries the signed int32 width of Darwin's struct Stat_t.Dev. It is
// widened through uint32 — NOT straight to uint64 — to match the zero-extension
// the kernel-buffer decode applies:
//
//	device = uint64(binary.LittleEndian.Uint32(buf[vnodePathInfoCwdDevOffset:]))
//
// Widening an int32 to uint64 directly sign-extends the high bit, so a
// filesystem whose device id has bit 31 set (>= 0x80000000 — FUSE mounts, Docker
// for Mac, some APFS volumes) would never compare equal to its own
// kernel-reported value. openWorkingDir would then return false for every
// process living on such a filesystem and reapClaimedWorktreeWriters would skip
// them, leaving worktree cleanup incomplete with no process reaped and no
// security weakened — a silent false negative (96e1a176).
func openedDirMatchesVnode(statDev int32, statIno uint64, statMode uint32, vnodeDevice, vnodeIno uint64, vnodeMode uint32) bool {
	return uint64(uint32(statDev)) == vnodeDevice &&
		statIno == vnodeIno &&
		statMode&unix.S_IFMT == vnodeMode&unix.S_IFMT
}

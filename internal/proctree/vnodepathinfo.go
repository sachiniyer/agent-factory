package proctree

import (
	"bytes"
	"encoding/binary"
)

// This file decodes darwin's PROC_PIDVNODEPATHINFO buffer — the kernel's answer
// to Linux's /proc/<pid>/cwd — and like procargs2.go it carries NO build tag,
// deliberately.
//
// The risk here is not that the decode is intricate; it is that it is
// UNFALSIFIABLE at a glance. The buffer is a 2352-byte C struct and the cwd
// lives at a fixed offset inside it, so a mis-declared offset does not fail —
// it returns a DIFFERENT STRING. That string is handed to reapWorktreeWriters
// (session/git/worktree_ops.go), which signals every process whose cwd is under
// the worktree. A fabricated path there is not a wrong report, it is a wrong
// kill: exactly the fabricated-positive this package exists to refuse (#1939),
// on the one call path where it is destructive.
//
// So the offsets are pinned here as named constants with their full derivation,
// the decode validates rather than trusts what it finds, and the whole thing is
// platform-independent so Linux CI exercises it on every run — the darwin
// syscall that feeds it stays in proctree_darwin.go. See #2050.

// Layout of struct proc_vnodepathinfo, transcribed from Apple's headers:
//
//	xnu bsd/sys/proc_info.h:
//	  struct vinfo_stat {
//	          uint32_t vst_dev;  uint16_t vst_mode; uint16_t vst_nlink;
//	          uint64_t vst_ino;  uid_t vst_uid;     gid_t vst_gid;
//	          int64_t  vst_atime, vst_atimensec, vst_mtime, vst_mtimensec,
//	                   vst_ctime, vst_ctimensec, vst_birthtime, vst_birthtimensec;
//	          off_t    vst_size; int64_t vst_blocks;
//	          int32_t  vst_blksize; uint32_t vst_flags, vst_gen, vst_rdev;
//	          int64_t  vst_qspare[2];
//	  };
//	  struct vnode_info      { struct vinfo_stat vi_stat; int vi_type; int vi_pad; fsid_t vi_fsid; };
//	  struct vnode_info_path { struct vnode_info vip_vi; char vip_path[MAXPATHLEN]; };
//	  struct proc_vnodepathinfo { struct vnode_info_path pvi_cdir, pvi_rdir; };
//
// with the typedef chain resolved from Apple's own headers rather than assumed:
// uid_t/gid_t are __uint32_t, off_t is __int64_t, fsid_t is
// `struct fsid { int32_t val[2]; }` (bsd/sys/_types/_fsid_t.h), and MAXPATHLEN
// is PATH_MAX (bsd/sys/param.h) which is 1024 (bsd/sys/syslimits.h).
//
// Every field is a fixed-width scalar or a char array — there is no long,
// pointer or long double — so natural alignment makes the layout identical on
// every LP64 target, i.e. the same on arm64 and amd64 macs. The numbers below
// were confirmed by compiling those exact declarations and printing
// sizeof/offsetof, not by hand arithmetic:
//
//	sizeof(vinfo_stat)                 = 136
//	sizeof(vnode_info)                 = 152
//	offsetof(vnode_info_path, vip_path)= 152
//	sizeof(vnode_info_path)            = 1176
//	sizeof(proc_vnodepathinfo)         = 2352
//
// pvi_cdir is the FIRST member, so the current directory's path begins at
// offset 152 and runs for MAXPATHLEN bytes. vnodepathinfo_test.go re-derives all
// of this from an independent field table so a typo in a constant fails a test
// rather than fabricating a path.
const (
	// vnodePathInfoMaxPathLen is MAXPATHLEN: the width of vip_path.
	vnodePathInfoMaxPathLen = 1024
	// vnodePathInfoSize is sizeof(struct proc_vnodepathinfo). The kernel writes
	// exactly this many bytes; readWorkingDir rejects any other count.
	vnodePathInfoSize = 2352
	// vnodePathInfoCwdPathOffset is offsetof(pvi_cdir.vip_path) — where the
	// working directory's NUL-terminated path starts.
	vnodePathInfoCwdPathOffset = 152
	// The first fields of pvi_cdir.vip_vi.vi_stat identify the cwd vnode.
	// Darwin uses these to reject a pathname replacement between proc_info and
	// open(2), before the handle is trusted by a destructive caller.
	vnodePathInfoCwdDevOffset  = 0
	vnodePathInfoCwdModeOffset = 4
	vnodePathInfoCwdInoOffset  = 8
)

// cwdFromVnodePathInfo extracts the working directory from a
// PROC_PIDVNODEPATHINFO buffer, reporting false rather than guessing.
//
// It VALIDATES instead of trusting the offset, because the caller is
// destructive. Three independent things must hold, and each one is a way a wrong
// layout gets caught rather than believed:
//
//   - the buffer is exactly sizeof(struct proc_vnodepathinfo). A kernel whose
//     struct is not the one described above writes a different count, and a
//     short buffer would otherwise be read past its meaningful end.
//   - the path is NUL-terminated within MAXPATHLEN. The kernel guarantees this
//     itself — proc_pidvnodepathinfo() in bsd/kern/proc_info.c ends with
//     `pvi_cdir.vip_path[MAXPATHLEN - 1] = 0` — so its absence means we are not
//     looking at vip_path.
//   - the path is absolute and free of control bytes. vn_getpath() always
//     returns a rooted path, while the bytes preceding vip_path are timestamps,
//     sizes and a fsid — small integers and zeros that essentially never spell a
//     plausible '/'-led string.
//
// A path this rejects degrades to the honest unknown, which every caller already
// handles as "cannot resolve" and skips. That asymmetry is the point: a false
// negative costs an unreaped writer (the status quo on darwin), a false positive
// costs the wrong process.
func cwdFromVnodePathInfo(buf []byte) (string, bool) {
	if len(buf) != vnodePathInfoSize {
		return "", false
	}
	raw := buf[vnodePathInfoCwdPathOffset : vnodePathInfoCwdPathOffset+vnodePathInfoMaxPathLen]
	end := bytes.IndexByte(raw, 0)
	if end <= 0 {
		// -1: no terminator, so this is not vip_path. 0: an empty path, which is
		// what a process with no cwd (a kernel task) reports — unknown, not "/".
		return "", false
	}
	path := raw[:end]
	if path[0] != '/' {
		return "", false
	}
	for _, c := range path {
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return string(path), true
}

func cwdIdentityFromVnodePathInfo(buf []byte) (path string, device, inode uint64, mode uint32, ok bool) {
	path, ok = cwdFromVnodePathInfo(buf)
	if !ok {
		return "", 0, 0, 0, false
	}
	device = uint64(binary.LittleEndian.Uint32(buf[vnodePathInfoCwdDevOffset:]))
	mode = uint32(binary.LittleEndian.Uint16(buf[vnodePathInfoCwdModeOffset:]))
	inode = binary.LittleEndian.Uint64(buf[vnodePathInfoCwdInoOffset:])
	return path, device, inode, mode, inode != 0
}

// vnodeDeviceFromFstatDev widens a darwin fstat(2) st_dev into the same uint64
// that cwdIdentityFromVnodePathInfo produces for vst_dev, so the two can be
// compared at all.
//
// The two sources describe the SAME 32-bit device id with opposite signedness.
// vinfo_stat.vst_dev is uint32_t, and the decode above widens it with
// binary.LittleEndian.Uint32 — zero-extension. darwin's struct stat spells that
// field dev_t, which is __int32_t (bsd/sys/_types/_dev_t.h), so
// x/sys/unix.Stat_t.Dev is int32 and a plain uint64(stat.Dev) SIGN-extends.
//
// Below the high bit the two agree and nothing is wrong. At or above it they
// never agree: darwin's makedev packs the major number into the top byte, so any
// major >= 128 sets bit 31, and a device id of 0x80000001 arrives as
// 0xffffffff80000001 from fstat and as 0x80000001 from the kernel. openWorkingDir
// then reads a mismatch that does not exist, refuses the handle, and its caller
// SKIPS the process (#3525).
//
// That failure direction is what makes it expensive rather than merely wrong.
// Skipping looks like the safe answer everywhere else in this package, but the
// callers are reapWorktreeWriters (#3510), which must see every process holding
// a worktree open before the tree is moved out from under it, and the reset path
// (#3519), which must see every process that survived SIGKILL. A writer this
// silently omits is not reported as unknown — it is not reported at all, on
// exactly the filesystems whose major number happens to be large, with no error
// anywhere.
//
// Truncating to uint32 first drops the sign extension and keeps the 32 bits the
// kernel actually reported. Untagged for the same reason as the rest of this
// file: the arithmetic is what is worth testing, and Linux CI can run it.
func vnodeDeviceFromFstatDev(dev int32) uint64 {
	return uint64(uint32(dev))
}

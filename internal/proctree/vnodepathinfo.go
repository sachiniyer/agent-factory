package proctree

import "bytes"

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

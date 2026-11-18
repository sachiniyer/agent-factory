package proctree

import (
	"bytes"
	"encoding/binary"
	"os"
	"syscall"
	"unsafe"
)

// procPIDPathInfo is PROC_PIDPATHINFO, the proc_info flavor returning the
// process's executable path directly — the same kernel-attested source Linux
// reads as /proc/<pid>/exe (#7491). It answers char buf[MAXPATHLEN] rather
// than a struct.
const procPIDPathInfo = 11

// procPIDPathInfoMaxSize is PROC_PIDPATHINFO_MAXSIZE from XNU's
// <sys/proc_info.h> — 4 * MAXPATHLEN. The kernel bounds the buffer to
// [MAXPATHLEN, MAXSIZE] (ENOMEM under, EOVERFLOW over); the top of the range
// is what vn_getpath_fsenter needs for the longest path it can return.
const procPIDPathInfoMaxSize = 4 * 1024

// procPIDRegionPathInfo2 is PROC_PIDREGIONPATHINFO2 (proc_info_private.h),
// the flavor that enumerates a process's VNODE-backed memory regions —
// fill_procregioninfo_onlymappedvnodes skips anonymous mappings — one region
// per call with `arg` as the address cursor. Each answer is a
// proc_regionwithpathinfo carrying the backing vnode's identity; the exe's
// own regions are backed by the text vnode, the kernel's record of which
// file the process is executing.
const procPIDRegionPathInfo2 = 22

// The proc_regionwithpathinfo byte layout the walk indexes lives in
// afexe_textvnode.go (untagged, like vnodepathinfo.go's offsets) so Linux
// CI re-derives it from the SDK field table on every run — see
// TestRegionWithPathInfoLayoutMatchesSDKHeaders.

// exePath asks the kernel for pid's executable path via proc_info, through
// the same procInfoRaw helper readCPUTime and openfiles use (Syscall6, never
// RawSyscall6 — proc_find can wait uninterruptibly mid-exec, #7241).
//
// PROC_PIDPATHINFO answers differently from the struct flavors: its retval is
// UNUSED — proc_pidpathinfo_internal never writes it — so a SUCCESSFUL read
// returns 0 with the NUL-terminated path copied into the buffer, exactly what
// libproc's proc_pidpath reads with strlen(buffer), not the return value
// (xnu bsd/kern/proc_info.c). Interpreting that 0 as "no answer" made every
// real af build unverifiable on darwin (#7492). The kernel refuses the call
// for processes we may not inspect via errno; a non-error answer whose first
// byte is NUL is no path at all. Both report "cannot resolve", which the
// attestation caller treats as unverified (fail-closed, never a verdict).
func exePath(pid int) (string, bool) {
	var buf [procPIDPathInfoMaxSize]byte
	_, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDPathInfo, 0,
		unsafe.Pointer(&buf[0]), procPIDPathInfoMaxSize)
	if err != nil {
		return "", false
	}
	end := bytes.IndexByte(buf[:], 0)
	if end <= 0 {
		return "", false
	}
	return string(buf[:end]), true
}

// exeTextVnode returns the (dev, ino) of the vnode backing pid's executable:
// the kernel's own record of what the process is executing, read by walking
// pid's vnode-mapped regions to the one carrying the kernel-attested exe
// path (xnu proc_pidregionpathinfo3 walks the same way). The walk is
// cursor-bounded; any refusal, short answer, stalled advance, no match, or
// AMBIGUOUS match reports false — fail-closed like every other
// "cannot resolve".
//
// AMBIGUITY. PROC_PIDREGIONPATHINFO2 reports ANY vnode-backed regular-file
// mapping at/after the cursor, in address order, without filtering on
// protection or binding to p_textvp (xnu bsd/kern/proc_info.c
// fill_procregioninfo_onlymappedvnodes / fill_vnodeinfoforaddr — a vnode
// pager exists only for a VREG, and pri_protection is copied, never
// checked). The process controls its own map, so the FIRST region whose
// vip_path string equals the kernel's exe path is not necessarily the text
// vnode: a process can mmap a DIFFERENT regular file — name-swapped onto
// the exe path — at an address below its own text region (x86_64 only; the
// arm64 hard 4 GiB pagezero leaves nothing mappable beneath the text). The
// walk therefore treats its answer as the text vnode only when UNSOLICITED:
//
//   - every region whose vip_path equals the exe path is collected, not
//     returned on first match;
//   - at least one must be PROT_EXEC (the executed image's own __TEXT);
//   - all must agree on the same (dev, ino) — a benign executable names
//     only its text vnode, across __TEXT/__DATA/__LINKEDIT (one inode); a
//     SECOND inode answering the same exe path is a foreign inode the
//     process mapped under the name: the only witness an unfiltered walk
//     has, and a forced fail-closed.
//
// Two or more distinct inodes naming the exe path — the poison — report
// false. A REPLACED-ONLY name (the legit upgradetxn case, exe renamed over
// while running) answers the text vnode alone — the replacement lives only
// on disk, never mapped under the running process — so the verdict still
// stands on the running image (#7492, #7494 item 2). exeOpen, exeIdentity,
// and the first attestation all read the SAME kernel-attested identity —
// never a path stat — so a name swap can never carry the cached positive
// across sweeps (#7509).
func exeTextVnode(pid int, exePath string) (dev uint64, ino uint64, ok bool) {
	var buf [procRegionWithPathInfoSize]byte
	addr := uint64(0)
	var matches []regionMatch
	for i := 0; i < regionScanBound; i++ {
		n, err := procInfoRaw(procInfoCallPIDInfo, pid, procPIDRegionPathInfo2, addr,
			unsafe.Pointer(&buf[0]), procRegionWithPathInfoSize)
		if err != nil || n < procRegionWithPathInfoSize {
			return 0, 0, false
		}
		regionEnd := binary.NativeEndian.Uint64(buf[regionPriAddressOff:]) +
			binary.NativeEndian.Uint64(buf[regionPriSizeOff:])
		path := cString(buf[regionPathOff : regionPathOff+regionPathLen])
		if path == exePath {
			matches = append(matches, regionMatch{
				dev:        uint64(binary.NativeEndian.Uint32(buf[regionVnodeDevOff:])),
				ino:        binary.NativeEndian.Uint64(buf[regionVnodeInoOff:]),
				protection: binary.NativeEndian.Uint32(buf[regionPriProtectionOff:]),
			})
		}
		if regionEnd <= addr {
			// End of walk: the kernel returns the last vnode-backed region
			// for a cursor past the end of the map (regionEnd <= addr),
			// so the cursor no longer advances. Fold whatever matches
			// the walk collected into one identity — empty (no region
			// names the exe path) collapses to the honest "foreign"
			// answer inside collectTextVnode, exactly the fail-closed
			// direction.
			return collectTextVnode(matches)
		}
		addr = regionEnd
	}
	// Region cap: a map this fragmented is itself suspicious, and a
	// consensus read that did not terminate before the cap cannot be
	// trusted — fail-closed, the same direction the unbounded-walk guard
	// already takes.
	return 0, 0, false
}

// exeOpen opens pid's executable on its inode. There is no /proc to shortcut
// this on darwin: the kernel's PROC_PIDPATHINFO answer is a path, and a path
// re-opened names whatever file holds that name NOW. So the open is
// verified instead — the file's (dev, ino) must equal the text vnode's
// (dev, ino) in pid's own memory map, the kernel's record of the executable
// it is running. A replaced path, an unreadable map, and any mismatch all
// report "cannot resolve" — the RUNNING inode is the only executable the
// verdict may read (#7303's reopen-by-path class, #7492).
func exeOpen(pid int) (*os.File, bool) {
	path, ok := exePath(pid)
	if !ok {
		return nil, false
	}
	dev, ino, ok := exeTextVnode(pid, path)
	if !ok {
		return nil, false
	}
	return openRegularExe(path, dev, ino)
}

// exeIdentity is the memo's revalidation read: the (dev,ino) of the TEXT
// VNODE — the inode the kernel is executing — from the same region walk
// the first attestation gates on, never a stat of the path name (#7509).
// A path stat can launder a dead verdict: exec a foreign binary at Q under
// a claimed argv, then `mv` the attested af image onto Q — stat(Q) returns
// the attested inode while the text vnode still belongs to the foreign
// image. The region walk reports the vnode by a path-STRING match, so the
// walk itself only stands as evidence when UNSOLICITED: it folds every
// region naming the exe path, requires at least one to be PROT_EXEC, and
// requires all to agree on one (dev,ino); a SECOND inode naming the path —
// a foreign inode the process mapped under the name — makes the fold
// fail-closed (see exeTextVnode). A replaced name (the legit upgradetxn
// case) names only the running text vnode, so the identity still stands on
// the running image; a name swap that maps a different inode under the
// path reports no identity instead. An exec, a replaced file, a deleted
// name, and any map that can no longer be read all change or lose the
// identity and force a re-attestation (#7496). A process too far gone to
// name reports no identity; the instance recheck lives in the shared
// caller.
func exeIdentity(pid int) (exeID, bool) {
	path, ok := exePath(pid)
	if !ok {
		return exeID{}, false
	}
	dev, ino, ok := exeTextVnode(pid, path)
	if !ok {
		return exeID{}, false
	}
	return exeID{dev: uint64(uint32(dev)), ino: ino}, true
}

// openRegularExe opens path and admits the file only when it is a REGULAR
// file whose (dev, ino) is the caller's — the text vnode's — identity.
// O_NONBLOCK is what makes that gate honest: a non-regular file renamed
// over the exe path — the FIFO wedge #7494 item 2 names — must refuse
// without ever parking on a writer that never comes, and a regular file is
// all buildinfo can read anyway. The whole gated open is one step so the
// refusal property is testable without staging a process behind a wedged
// name.
func openRegularExe(path string, dev, ino uint64) (*os.File, bool) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Ino != ino || uint64(uint32(sys.Dev)) != dev {
		f.Close()
		return nil, false
	}
	return f, true
}

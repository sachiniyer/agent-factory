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

// Layout of struct proc_regionwithpathinfo (bsd/sys/proc_info.h), derived in
// the same spirit as vnodepathinfo.go: proc_regioninfo is 96 bytes
// (pri_address at 80, pri_size at 88), then vnode_info_path — vi_stat
// (vst_dev uint32 @96, vst_ino uint64 @104, inside the 136-byte stat block)
// then vip_path[MAXPATHLEN] at 96+152 = 248.
const (
	procRegionWithPathInfoSize = 1272
	regionPriAddressOff        = 80
	regionPriSizeOff           = 88
	regionVnodeDevOff          = 96
	regionVnodeInoOff          = 104
	regionPathOff              = 248
	regionPathLen              = 1024
	// regionScanBound caps the cursor walk so a hostile or pathological map
	// can never make the attestation read unbounded.
	regionScanBound = 16384
)

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
// cursor-bounded; any refusal, short answer, stalled advance, or a map with
// no region naming that path reports false — fail-closed like every other
// "cannot resolve".
func exeTextVnode(pid int, exePath string) (dev uint64, ino uint64, ok bool) {
	var buf [procRegionWithPathInfoSize]byte
	addr := uint64(0)
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
			return uint64(binary.NativeEndian.Uint32(buf[regionVnodeDevOff:])),
				binary.NativeEndian.Uint64(buf[regionVnodeInoOff:]), true
		}
		if regionEnd <= addr {
			return 0, 0, false
		}
		addr = regionEnd
	}
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

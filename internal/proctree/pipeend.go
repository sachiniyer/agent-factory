package proctree

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Pipe-end identity, and which processes hold a given pipe end (#7121).
//
// These exist so a reap can name the processes holding a command's pipes
// instead of signalling the command's whole process group. A group member
// that holds none of the command's pipes is not evidence of a leak. Under
// ControlMaster/ControlPersist, OpenSSH leaves the master's ProxyCommand in
// the step's group, with its stderr on /dev/null and its stdin and stdout on
// pipes to the master. Killing it kills the master. A member that still holds
// one of the command's own pipes after the command exited is the straggler a
// reap exists for, and that is a fact the kernel can be asked per process.
//
// The answer is "the truth or an error", never a guess, like OpenFilePaths: a
// process provably holding none of the ends returns false and nil, and a
// process that cannot be read returns an error. A caller deciding whether to
// signal must treat that error as "not proven", never as "holds it".
//
// This file carries no build tag so that Linux CI also exercises the decode of
// darwin's buffer (pipeHandleFromPipeFdInfo), as vnodepathinfo.go does for the
// cwd decode. A mis-declared offset there would not fail. It would return a
// different number, and that number decides a kill.

// PipeEndID returns the identity of the pipe end f, for HoldsPipeEnd.
//
// It is the end's inode number as fstat reports it. On Linux both ends of a
// pipe share one pipefs inode, which /proc/<pid>/fd shows as "pipe:[N]". On
// darwin each end has its own: XNU's pipe_stat reports
// VM_KERNEL_ADDRHASH(cpipe) for the end's own struct pipe, and fill_pipeinfo
// reports the same hash as pipe_handle (bsd/kern/sys_pipe.c). So the identity
// to take is that of the end the other process holds: the write end of a
// pipe it writes to, the read end of one it reads from.
//
// Stat, not Fd: Fd puts a pollable file into blocking mode, and the caller
// may still be using deadlines on it.
func PipeEndID(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if fi.Mode()&fs.ModeNamedPipe == 0 {
		return 0, fmt.Errorf("proctree: %s is not a pipe (mode %v)", f.Name(), fi.Mode())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("proctree: no stat_t for %s", f.Name())
	}
	return uint64(st.Ino), nil
}

// HoldsPipeEnd reports whether pid holds a descriptor on any of the pipe ends
// ids names, each one taken from PipeEndID.
//
// ErrProcessExited (or a process-table miss) means the process is gone. Any
// other error means it could not be read: a process the kernel will not let
// this user inspect, such as one that made itself non-dumpable. Neither is a
// "yes".
func HoldsPipeEnd(pid int, ids ...uint64) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	want := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	return holdsPipeEnd(pid, want)
}

// GroupMembers returns every live process whose process group is pgid, read
// from the kernel: /proc/<pid>/stat's pgrp field on Linux, kern.proc.pgrp on
// darwin. Zombies are left out. They hold no descriptors and answer no
// signal.
//
// A non-positive pgid matches nothing, for the reason SessionMembers gives:
// the result can feed a kill.
//
// On darwin the read can block, uninterruptibly, while a member is part-way
// through an exec (proc_iterate calls proc_transwait), and it is not a
// snapshot: a child still being created is left out. A caller that acts on the
// answer bounds the read in wall-clock time and reads again before it stops.
func GroupMembers(pgid int) ([]Process, error) {
	if pgid <= 0 {
		return nil, nil
	}
	return groupMembers(pgid)
}

// errNotPipeLink reports a /proc fd link that names something other than a
// pipe.
var errNotPipeLink = errors.New("not a pipe link")

// pipeInodeFromLink decodes the target of a /proc/<pid>/fd/N link that names a
// pipe, "pipe:[12345]", into its inode number.
func pipeInodeFromLink(target string) (uint64, error) {
	rest, ok := strings.CutPrefix(target, "pipe:[")
	if !ok {
		return 0, errNotPipeLink
	}
	digits, ok := strings.CutSuffix(rest, "]")
	if !ok || digits == "" {
		return 0, errNotPipeLink
	}
	ino, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, errNotPipeLink
	}
	return ino, nil
}

// Layout of struct pipe_fdinfo, the buffer proc_pidfdinfo(PROC_PIDFDPIPEINFO)
// fills, transcribed from xnu bsd/sys/proc_info.h (xnu-11417.140.69):
//
//	struct proc_fileinfo {
//	        uint32_t fi_openflags; uint32_t fi_status; off_t fi_offset;
//	        int32_t fi_type; uint32_t fi_guardflags;
//	};                                                  // 24 bytes
//	struct vinfo_stat { ... };                          // 136 bytes, see vnodepathinfo.go
//	struct pipe_info {
//	        struct vinfo_stat pipe_stat;                // +0
//	        uint64_t pipe_handle;                       // +136
//	        uint64_t pipe_peerhandle;                   // +144
//	        int pipe_status; int rfu_1;                 // +152, +156
//	};                                                  // 160 bytes
//	struct pipe_fdinfo { struct proc_fileinfo pfi; struct pipe_info pipeinfo; };
//
// So pipe_handle sits at 24 + 136 = 160, and the whole struct is 184 bytes.
// pid_pipeinfo copies out exactly sizeof(struct pipe_fdinfo) and reports that
// as the length (bsd/kern/proc_info.c).
const (
	pipeFdInfoSize         = 184 // sizeof(struct pipe_fdinfo)
	pipeFdInfoHandleOffset = 160 // offsetof(pipeinfo.pipe_handle)
)

// pipeHandleFromPipeFdInfo decodes pipe_handle out of a pipe_fdinfo buffer.
// A buffer of the wrong length, or a zero handle, is refused: a hash of a live
// struct pipe is never zero, and a zeroed or short answer must read as "could
// not see", never as an identity that might match something.
func pipeHandleFromPipeFdInfo(buf []byte) (uint64, bool) {
	if len(buf) != pipeFdInfoSize {
		return 0, false
	}
	h := binary.LittleEndian.Uint64(buf[pipeFdInfoHandleOffset:])
	if h == 0 {
		return 0, false
	}
	return h, true
}

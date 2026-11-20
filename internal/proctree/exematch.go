package proctree

import (
	"errors"
	"syscall"
)

// ExeMatch is ExeMatchesImage's answer about whether the kernel attests
// that pid is running a particular image. Four answers because "could not
// tell" and "no process" are facts of their own, not the negative — the
// same discipline TmuxIdentity carries (#7238), plus the gone state a live
// process table owes.
type ExeMatch int

const (
	// ExeMatchOther: the kernel attests a DIFFERENT image — the one
	// conclusive answer this check can give against a process.
	ExeMatchOther ExeMatch = iota
	// ExeMatchSame: the kernel attests the reference's own inode — the
	// process IS running that executable.
	ExeMatchSame
	// ExeMatchUnknown: the image could not be read — EACCES/EPERM, a stalled
	// mount, any refusal short of gone. Identity unproven, never disproven.
	ExeMatchUnknown
	// ExeMatchGone: ENOENT/ESRCH — there is no live process to identify.
	ExeMatchGone
)

// ExeImage is a file resolved to what the kernel's per-process executable
// records attest: device and inode, never a name. Renaming the path,
// `exec -a`, a symlink or a hardlink cannot change the image a process
// runs; a renamed COPY is another inode, which is the one identity this
// comparison cannot reach. dev is normalized to its low 32 bits on both
// sides of the comparison — the width darwin's vnode_info reports — so the
// fields are only ever compared to each other, never read directly.
type ExeImage struct{ dev, ino uint64 }

// ExeImageOf resolves a file's path to the identity the kernel's exe
// records carry for a process running it: one stat, through symlinks —
// the same inode /proc/<pid>/exe names on Linux and the text vnode names
// on darwin.
func ExeImageOf(path string) (ExeImage, error) {
	return exeImageOfFile(path)
}

// exeMatchRead is the kernel's per-process image read, behind a package
// var so the verdict table can be pinned without staging process states.
var exeMatchRead = exeImageForMatch

// errExeImageUnreadable marks every "the image could not be established"
// outcome that is not an errno from the kernel — an empty path answer, a
// vnode walk that could not finish, a platform with no exe backend. It
// classifies as Unknown (unproven), never as Gone.
var errExeImageUnreadable = errors.New("executable image could not be established")

// ExeMatchesImage reports whether the kernel attests pid is running ref's
// image — the executable identity an argv[0] rename, `exec -a`, symlink or
// hardlink cannot change (a renamed COPY is another inode and answers
// Other). A refused read reports Unknown — EACCES/EPERM means unproven,
// not disproven — and ENOENT/ESRCH reports Gone, because a dead process
// holds nothing a caller could be asking after.
func ExeMatchesImage(pid int, ref ExeImage) ExeMatch {
	id, err := exeMatchRead(pid)
	switch {
	case err == nil:
		if id == ref {
			return ExeMatchSame
		}
		return ExeMatchOther
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ESRCH):
		return ExeMatchGone
	default:
		return ExeMatchUnknown
	}
}

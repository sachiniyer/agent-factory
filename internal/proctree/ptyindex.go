package proctree

import (
	"strconv"
	"strings"
)

// PtyIndex names one pty allocation: the index a pty pair is known by inside
// its terminal instance plus that instance's identity, because the index
// alone is not unique — two tmux servers on separate devpts mounts (a host
// and a container, say) each mint their own /dev/pts/0. The Dev half keeps a
// carrier's slave matched only to the master of its own pair (Codex on
// #7469); on darwin, where devfs is single-instance, it is 0.
type PtyIndex struct {
	Dev   uint64
	Index int
}

// PtySlaveIndices returns the pseudo-terminal SLAVE identities pid holds
// open: every /dev/pts/N link (linux) or /dev/ttysNNN vnode (darwin) among
// its descriptors contributes its N. A pane process's stdio is its pane's
// slave; a detached carrier that kept its stdio still holds it, and the
// identity names the pty pair whose MASTER side some tmux server holds —
// the only kernel link a reparented process keeps to the server it left
// (#7469).
//
// An error means the descriptor table could not be READ — "could not see",
// never "holds none". A process that holds no slave reports an empty slice.
func PtySlaveIndices(pid int) ([]PtyIndex, error) {
	return ptySlaveIndices(pid)
}

// PtyFdIndex pairs a pty-master identity with the descriptor the holder
// keeps it on — the record a cached ownership map re-verifies through
// (#7581 item 2). Fd is the read-time descriptor number; a closed or
// reused descriptor fails the re-verify and contributes nothing.
type PtyFdIndex struct {
	PtyIndex
	Fd int
}

// PtyMasterIndices returns the pseudo-terminal MASTER identities pid holds
// open: the tty-index of each ptmx fd (linux fdinfo) or each /dev/ptmx
// vnode's rdev minor (darwin), qualified by the terminal instance exactly
// like the slave side. A tmux server holds one master per live pane;
// pairing these with a carrier's PtySlaveIndices is what attributes the
// carrier's stdio to the server that created it.
//
// Same contract as PtySlaveIndices: an error is "could not see".
func PtyMasterIndices(pid int) ([]PtyIndex, error) {
	fds, err := ptyMasterFdIndices(pid)
	if err != nil {
		return nil, err
	}
	out := make([]PtyIndex, len(fds))
	for i, fi := range fds {
		out[i] = fi.PtyIndex
	}
	return out, nil
}

// PtyMasterFdIndices is PtyMasterIndices carrying the descriptor number of
// each master — what the sweep's ownership map stores so a later verdict
// can re-verify exactly the recorded fd instead of re-walking the holder's
// whole fd table (#7581 item 2).
func PtyMasterFdIndices(pid int) ([]PtyFdIndex, error) {
	return ptyMasterFdIndices(pid)
}

// PtyMasterFdIndex re-verifies ONE recorded descriptor: the pty-master
// identity pid's fd currently holds, with ok reporting the read answered
// AND the fd is still a ptmx master. A descriptor that closed, was reused
// for another kind of file, or whose table cannot be read answers false —
// the caller treats it as "the cached claim proved nothing": undecidable,
// never foreign (#7581 item 2). One fdinfo file (linux) or one vnode
// decode (darwin) — never a walk of the holder's table.
func PtyMasterFdIndex(pid, fd int) (PtyIndex, bool) {
	return ptyMasterFdIndex(pid, fd)
}

// ptySlaveIndexFromPath parses a /dev/pts/N link target (linux) or a
// /dev/ttysNNN vnode path (darwin) into its index. The name's numeric tail IS
// the index on both platforms; anything else is not a pty slave.
// Untagged so Linux CI exercises both spellings.
func ptySlaveIndexFromPath(target string) (int, bool) {
	if rest, ok := strings.CutPrefix(target, "/dev/pts/"); ok {
		idx, err := strconv.Atoi(rest)
		return idx, err == nil && idx >= 0
	}
	if rest, ok := strings.CutPrefix(target, "/dev/ttys"); ok {
		idx, err := strconv.Atoi(rest)
		return idx, err == nil && idx >= 0
	}
	return 0, false
}

// ptyMasterIndexFromFdinfo parses one /proc/<pid>/fdinfo/<fd> body for its
// tty-index line — the field only a ptmx master fd carries (linux). The bool
// reports whether the line was present, so a non-tty fd answers (0, false)
// rather than an error.
func ptyMasterIndexFromFdinfo(body string) (int, bool) {
	for line := range strings.Lines(body) {
		if rest, ok := strings.CutPrefix(line, "tty-index:"); ok {
			idx, err := strconv.Atoi(strings.TrimSpace(rest))
			return idx, err == nil && idx >= 0
		}
	}
	return 0, false
}

package proctree

import (
	"strconv"
	"strings"
)

// PtySlaveIndices returns the pseudo-terminal SLAVE indices pid holds open:
// every /dev/pts/N link (linux) or /dev/ttysNNN vnode (darwin) among its
// descriptors contributes its index N. A pane process's stdio is its pane's
// slave; a detached carrier that kept its stdio still holds it, and the index
// names the pty pair whose MASTER side some tmux server holds — the only
// kernel link a reparented process keeps to the server it left (#7469).
//
// An error means the descriptor table could not be READ — "could not see",
// never "holds none". A process that holds no slave reports an empty slice.
func PtySlaveIndices(pid int) ([]int, error) {
	return ptySlaveIndices(pid)
}

// PtyMasterIndices returns the pseudo-terminal MASTER indices pid holds open:
// the tty-index of each ptmx fd (linux fdinfo) or each /dev/ptmx vnode's rdev
// minor (darwin). A tmux server holds one master per live pane; pairing these
// with a carrier's PtySlaveIndices is what attributes the carrier's stdio to
// the server that created it.
//
// Same contract as PtySlaveIndices: an error is "could not see".
func PtyMasterIndices(pid int) ([]int, error) {
	return ptyMasterIndices(pid)
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

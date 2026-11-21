//go:build linux

package tmux

import (
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// probeSocketOwners is the Linux census: the kernel's own unix-socket
// tables decide who could own the absent path (#7695), replacing the #7689
// title candidacy, which could never name a renamed server and which a
// non-dumpable process could hold "unknown" forever. A bound socket is a
// row in the table of the network namespace it was bound in — spelled as
// bind() was given it, surviving the file's unlink — so the tables,
// grouped by netns across every live same-uid process, answer the
// ownership question directly, and an attributed inode names the holder.
//
// The verdict this feeds keeps #7562's certain-versus-undecidable split: a
// row whose bound spelling provably names this socket is the owner; one
// the spelling walk cannot decide stays a possible owner, signalled never;
// one provably elsewhere is left out. A path af cannot even place — the
// client spelled it relative — is "could not probe", claimed, never a
// determinate empty.
func probeSocketOwners(socketPath string) socketSweep {
	want, ok := canonicalSocketPath(socketPath)
	if !ok {
		return socketSweep{unprobed: true}
	}
	us := proctree.UnixSocketOwnerSweep(boundRowMatcher(socketPath, want))
	s := socketSweep{
		bound:                 map[int]boundOwner{},
		uncertain:             map[int]string{},
		unattributedBound:     us.UnattributedBound,
		unattributedUncertain: us.UnattributedUncertain,
		uncovered:             us.Uncovered,
		unprobed:              us.Unprobed,
	}
	for pid, spelling := range us.Bound {
		s.bound[pid] = boundOwner{spelling: spelling, source: "the kernel's socket tables"}
	}
	for pid, spelling := range us.Uncertain {
		s.uncertain[pid] = spelling
	}
	return s
}

// boundRowMatcher is the sweep's per-row verdict: does this kernel-spelled
// bound path certainly name the absent socket (the row's holder is the
// owner), possibly name it (the holder is a possible owner, never
// signalled), or provably name something else (ruled out). It is
// bindingAgainst's exact semantics, per row rather than per server —
// including the literal-bound-path premise (the kernel keeps the bind()
// spelling verbatim, so got needs no alias benefit) and the
// certain-versus-undecidable-want split of #7562.
//
// A basename compare runs first: certainSocketMismatch's own tail rule —
// for a certain want every unequal pair is provable, for an uncertain one
// the tail's last name — decides most rows without the syscall walk
// canonicalSocketPath spends on the rest. Rows it cannot predecide pay the
// walk.
func boundRowMatcher(socketPath string, want socketPathWalk) func(string) proctree.RowMatch {
	filterName, filtering := socketMatchFilterName(want)
	return func(boundPath string) proctree.RowMatch {
		if filtering {
			if _, base := splitSocketPath(boundPath); base != filterName {
				return proctree.RowMatchNone
			}
		}
		got, ok := canonicalSocketPath(boundPath)
		if !ok {
			// A bound spelling that will not place — a link loop — cannot be
			// decided either way.
			return proctree.RowMatchPossible
		}
		if got.path == want.path || sameSocketDir(boundPath, socketPath) {
			return proctree.RowMatchCertain
		}
		if !certainSocketMismatch(want, got) {
			return proctree.RowMatchPossible
		}
		return proctree.RowMatchNone
	}
}

// socketMatchFilterName is the one name a row's basename must equal before
// the full walk is worth running — and false when no such name exists. For
// a certain want it is the wanted path's own basename: every unequal pair
// is provably a different socket, so a differing basename is out. For an
// uncertain want it is the plain tail's last component, exactly the name
// certainSocketMismatch itself can prove against; a non-plain tail decides
// nothing, so nothing is filtered.
func socketMatchFilterName(want socketPathWalk) (string, bool) {
	if want.certain() {
		_, base := splitSocketPath(want.path)
		switch base {
		case "", ".", "..":
			return "", false
		}
		return base, true
	}
	tail := want.rest[1:]
	if len(tail) == 0 || !plainSocketComponents(tail) {
		return "", false
	}
	return tail[len(tail)-1], true
}

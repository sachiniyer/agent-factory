//go:build linux

package tmux

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// unixSocketOwnerSweep is proctree.UnixSocketOwnerSweep as a package var so
// a verdict test can stand a synthetic kernel census in for the real one —
// the foreign-mount-namespace row a shared box cannot stage is one (#7695).
var unixSocketOwnerSweep = proctree.UnixSocketOwnerSweep

// procSameFileView and procWorkingDir are the holder-evidence reads pending
// rows wait on, as package vars for the same reason: a same-uid process in
// a foreign mount namespace is not a fixture a test may conjure without
// userns privileges a shared box need not have.
var (
	procSameFileView = proctree.SameFileView
	procWorkingDir   = proctree.WorkingDir
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
// one provably elsewhere is left out — but "provably" means provably in
// the HOLDER'S filesystem view, not af's: a row whose basename matches is
// ruled out only when every attributed holder provably shares af's mount
// namespace and root (R1), and a relative spelling is placed against its
// holder's working directory or not at all (R2). A row af cannot place
// and a row whose holder's view af cannot establish both stay possible
// owners — claimed, never a determinate empty.
func probeSocketOwners(socketPath string) socketSweep {
	want, ok := canonicalSocketPath(socketPath)
	if !ok {
		return socketSweep{unprobed: true}
	}
	us := unixSocketOwnerSweep(boundRowMatcher(socketPath, want))
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
	for _, pr := range us.Pending {
		resolvePendingRow(&s, pr, socketPath, want, us.ScanComplete)
	}
	return s
}

// resolvePendingRow folds one row the match deferred into the verdict. A
// mismatch row — af's own filesystem proved the spelling names a different
// socket — is ruled out only when EVERY attributed holder provably shares
// af's mount namespace and root, and only when attribution was complete:
// the binder's view is what bind() recorded, and one holder whose view
// cannot be established — or nobody reached at all — keeps the whole row
// a possible tie (R1). A relative row may be placed against a same-view
// holder's working directory; when one resolves it to the queried path the
// row is decided bound and every holder of the socket is an owner —
// otherwise it stays a possible owner, since a holder can have moved since
// bind() (R2). An unattributed row names no holder to ask and so is never
// ruled out either way.
func resolvePendingRow(s *socketSweep, pr proctree.PendingRow, socketPath string, want socketPathWalk, scanComplete bool) {
	if len(pr.Holders) == 0 {
		s.unattributedUncertain++
		return
	}
	switch pr.Kind {
	case proctree.PendingMismatch:
		if !scanComplete {
			// A truncated scan may have missed the holder whose view is
			// foreign — never a rule-out on partial evidence.
			for _, h := range pr.Holders {
				s.uncertain[h] = pr.Spelling
			}
			return
		}
		for _, pid := range pr.Holders {
			if !procSameFileView(pid) {
				for _, h := range pr.Holders {
					s.uncertain[h] = pr.Spelling
				}
				return
			}
		}
		// Every holder provably shares af's view: the spelling af proved
		// elsewhere is elsewhere in the only view that matters. Ruled out.
	case proctree.PendingRelative:
		for _, pid := range pr.Holders {
			if !procSameFileView(pid) {
				continue
			}
			cwd, ok := procWorkingDir(pid)
			if !ok {
				continue
			}
			joined := strings.TrimSuffix(cwd, "/") + "/" + pr.Spelling
			got, ok := canonicalSocketPath(joined)
			if !ok || (got.path != want.path && !sameSocketDir(joined, socketPath)) {
				continue // unresolved, or placed elsewhere — a moved cwd decides nothing
			}
			source := "the kernel's socket tables, resolved through pid " + strconv.Itoa(pid) + "'s working directory"
			for _, h := range pr.Holders {
				s.bound[h] = boundOwner{spelling: pr.Spelling, source: source}
			}
			return
		}
		for _, h := range pr.Holders {
			s.uncertain[h] = pr.Spelling
		}
	}
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
// walk — but the walk only ever answers in AF'S view. Two outcomes are
// therefore deferred to the row's holder, which the sweep names after
// attribution (and only for rows that passed this basename filter, so the
// negative path pays nothing extra): a spelling af proved elsewhere is a
// mismatch pending the holder's view (R1), and a relative spelling af
// cannot place at all is pending the holder's cwd (R2) — never af's own.
//
// The raw-basename compare that licenses the cheap rule-out proves a row
// names a different socket ONLY when neither end's final component is a
// present symlink. canonicalSocketPath walks a present symlink at the
// basename — its own doc — so a symlink there redirects the spelling's
// canonicalized basename (the queried path's filter name becomes the link
// target's basename, and a row spelled identically to the queried path, or
// through an alias whose own symlink still sits at its basename, converges
// with the queried path under walk, not under raw compare). The #2875
// protection's premise — a /tmp cleaner can unlink the socket under a live
// server — is the absent basename, the case this prefilter was made for;
// a hand-placed or rerouting symlink at that basename is the residual that
// makes the raw compare prove more than the walk would, and so such a
// basename keeps the prefilter from ruling a row out before the walk can
// decide. The plain cases — a live socket file at the basename, the absent
// file the probe was called for, a directory, or a parent that does not
// exist — read not-a-symlink and keep the cheap rule-out.
func boundRowMatcher(socketPath string, want socketPathWalk) func(string) proctree.RowMatch {
	filterName, filtering := socketMatchFilterName(want)
	queryBasenameRedirected := socketBasenameSymlinked(socketPath)
	return func(boundPath string) proctree.RowMatch {
		if filtering {
			if _, base := splitSocketPath(boundPath); base != filterName &&
				!queryBasenameRedirected && !socketBasenameSymlinked(boundPath) {
				return proctree.RowMatchNone
			}
		}
		if !filepath.IsAbs(boundPath) {
			// A relative spelling placed against af's cwd is af's own
			// fiction — the holder's cwd is the only cwd that was in scope
			// at bind(). The holder decides.
			return proctree.RowMatchRelative
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
		// Provably elsewhere IN AF'S VIEW — but the spelling is the
		// binder's view's record, and a holder in a foreign mount
		// namespace may have bound af's path under its own spelling.
		// The holder decides.
		return proctree.RowMatchMismatch
	}
}

// socketBasenameSymlinked reports whether p's final component is a present
// symlink — the one shape under which canonicalSocketPath changes a
// spelling's basename, by following that link to its target. The basename
// prefilter in boundRowMatcher rules a row out on a raw-basename mismatch
// only when neither the queried path nor the row is so redirected: a
// present symlink at either end makes two raw basenames that differ able to
// converge post-walk, the case the walk — not the prefilter — must decide.
// A regular file (a live socket), the absent file the probe was called
// for, a directory, or a parent that does not exist, all read
// not-a-symlink and so keep the cheap rule-out.
func socketBasenameSymlinked(p string) bool {
	dir, base := splitSocketPath(p)
	if dir == "" || base == "" {
		return false
	}
	fi, err := os.Lstat(dir + "/" + base)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
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

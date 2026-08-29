package git

import (
	"errors"
	"io/fs"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
)

// relocateBeforeWriterReap is a test seam for the window between resolving the
// relocation source and proving it is still the same directory. It mirrors
// repoGoneBeforeWriterReap on the repo-gone path, which exists for the same
// reason: the replacement race below cannot otherwise be produced on demand.
var relocateBeforeWriterReap = func(string) {}

// reapRelocationSourceWriters kills every live process still working inside a
// worktree that is about to be RELOCATED, and returns the pathname the
// relocation is about to vacate so the caller can check that pathname for
// residue once the bytes have landed. An empty return means nothing is being
// vacated and no residue check is owed.
//
// # Why a MOVE needs the same reap a REMOVE has
//
// reapWorktreeWriters was written for the remove path (#2025), where a live
// writer announces itself: `git worktree remove -f` and the os.RemoveAll
// fallback both fail "directory not empty" when files are being re-created
// faster than they can be unlinked. That loud failure is what earned removal a
// reap. A relocation has the same hazard and none of the noise — the rename
// SUCCEEDS, and the writer is simply left pointed at a pathname that no longer
// holds the tree (#3391). Two archives in this repo's own daemon log did exactly
// that: a post-worktree `make dev_install` turbo build was still running when
// the session was archived, and it died with
//
//	Error: ENOENT: no such file or directory, open '<vacated path>/apps/frontend/.next/required-server-files.json'
//
// which reads as a broken build — on a Dependabot review, a wrong verdict on a
// dependency bump — rather than as af moving the tree. Both vacated pathnames
// still hold a `.git`-less skeleton today, because the doomed build re-created
// them by absolute path after the move.
//
// # Why archive REAPS rather than REFUSES
//
// Refusing to relocate while writers are live is the fail-closed shape this repo
// reaches for when ownership cannot be proven (CheckWorktreeOccupants), and it
// is the wrong shape here, for three reasons:
//
//   - The panes are already gone. Archive's teardown closes every tab and waits
//     for each pane to exit BEFORE the worktree step (#802/#1917), the same
//     precondition that lets worktreeReapGrace be zero. A process still writing
//     here has therefore already escaped its pane, its terminal is destroyed,
//     and nothing is reading its output — so it cannot finish usefully, only
//     corrupt. Refusing at this point would leave a session with its panes
//     killed and its worktree unmoved, which is worse than either alternative.
//   - A refusal earlier cannot discriminate. Before teardown, every live
//     session's own agent is cwd'd in its worktree, so a writers-live gate would
//     refuse every archive.
//   - Archive is the non-destructive lifecycle action and must stay usable. So
//     the reap itself is best-effort: reapWorktreeWriters degrades to a no-op on
//     an unreadable process table, and a writer that survives SIGKILL is left to
//     the residue report below rather than turning a working archive into a hard
//     failure.
//
// Ownership is proven the same way the remove path proves it, and deliberately
// NOT by the AF_SESSION marker sweep. A cwd at or under an af-created,
// session-private worktree path is itself the proof; the marker sweep exists for
// evidence gathered from the process table at large, where a same-named process
// could belong to anything. External/in-place worktrees — the user's own working
// tree — never reach here: relocateWorktreeTo refuses them before this point.
//
// # The claim is revalidated FIRST, and that is not optional
//
// Selecting processes by PATHNAME is destructive, so it needs the ownership
// proof at its front that the remove path's reap was given in #3278: "the writer
// reap terminates every process under the path, which is itself destructive
// against a replacement directory". Source resolution is a point-in-time claim,
// and the engine's later revalidation — at the fast-move boundary — would notice
// a same-uid replacement only AFTER this reap had already signalled processes
// belonging to it. Proving identity here is what keeps the blast radius inside
// the directory af actually claimed.
//
// It applies to every relocation role rather than to archive alone, because the
// hazard belongs to the MOVE and not to the caller's intent. Attaching the
// discipline to one call site is what produced #3391 in the first place.
func (g *GitWorktree) reapRelocationSourceWriters(src, dest string, claim RelocationClaim) (string, error) {
	if src == "" || src == dest {
		return "", nil
	}
	relocateBeforeWriterReap(src)
	if err := g.RevalidateRelocationClaim(claim); err != nil {
		return "", err
	}
	reapWorktreeWriters(src)
	return src, nil
}

// reportRelocationResidue warns when the pathname a relocation vacated exists
// again afterwards. That can only be a writer which outlived the reap above and
// re-created the path by absolute name, and it matters beyond tidiness: the
// leftover is not registered with git, is not part of the relocated worktree,
// and sits exactly where a later session deriving the same path would land.
//
// It reports and never removes. af vacated this pathname, so a directory that
// exists there now was created by something else, and deleting a stranger's
// files to tidy up is the trade this repo does not make — the same call the
// leftover-source warning beside it makes, with the same by-hand suggestion.
//
// It also declines to say the RELOCATED tree is fine, and points at it instead.
// The reap is best-effort by design — an unreadable process table reaps nothing,
// and a writer in uninterruptible I/O survives SIGKILL — and a survivor keeps its
// working directory, which after a rename resolves to the tree at its NEW
// location. So residue at the vacated pathname (an absolute-path write) is
// evidence that something is also positioned to write RELATIVE paths straight
// into the destination. relocated names it so the operator can check both.
//
// The probe is BOUNDED, because of where it runs: the bytes and the git
// registration are already committed and the caller has not returned yet, so an
// unbounded os.Stat against a filesystem that stalled during the move would
// wedge teardown after its result was established but before anything could
// finalize it — the exact class #1917 bounded the rest of this path for. A stall
// is therefore reported, not waited on.
//
// And a probe that could not answer is NOT an answer of "clear". Only a genuine
// "does not exist" is silence; every other failure says so, because a check that
// never ran must not read as a check that passed.
func reportRelocationResidue(vacated, relocated string) {
	if vacated == "" {
		return
	}
	if _, err := boundedRelocationPathIdentity(vacated); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		log.WarningLog.Printf(
			"worktree was relocated away from %s, but whether that pathname is now clear could not be "+
				"established. Check it by hand: anything a writer left there is not registered with git, "+
				"and a later session deriving the same path would inherit it: %v",
			vacated, err,
		)
		return
	}
	log.WarningLog.Printf(
		"worktree was relocated away from %s, but that pathname exists again: a writer that outlived the "+
			"pre-move reap re-created it by absolute path. That leftover is no part of the relocated "+
			"worktree and is not registered with git — inspect it and remove it by hand with `%s`, because "+
			"a later session deriving the same path would inherit it. Check the relocated worktree at %s "+
			"too: the same writer still holds a working directory, which after the move resolves to the "+
			"tree at its new location, so its relative writes land there.",
		vacated, shellsuggest.Command("rm", "-rf", vacated), relocated,
	)
}

package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"golang.org/x/sys/unix"
)

// RestoreWorktreePath returns where an archived session's worktree should be
// restored, honoring the configured worktree_root placement (#1540) exactly as
// NewGitWorktree does at creation — routing through the shared
// resolveWorktreePlacement. Sibling mode returns {repoParent}/{repoName}-
// {safeTitle}; subdirectory mode returns {AF_HOME}/worktrees/{branchName}, so a
// subdirectory user gets the worktree back where it belongs instead of stranded
// beside the repo. branchName is the session's persisted branch (used only for
// subdirectory placement). A numeric suffix is appended if the path is occupied,
// and the result is validated to sit strictly inside the worktree parent (#461).
func RestoreWorktreePath(repoPath, title, branchName string) (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	repoRoot, err := findGitRepoRoot(repoPath)
	if err != nil {
		return "", err
	}
	worktreeDir, err := getWorktreeDirectoryForRepoWithConfig(cfg, repoRoot)
	if err != nil {
		return "", err
	}
	return resolveWorktreePlacement(cfg, repoRoot, worktreeDir, title, branchName)
}

// ErrRepoGone is returned by RestoreWorktreeTo when the origin repository this
// worktree is registered against no longer exists (deleted, unmounted, or no
// longer a git repository). A worktree cannot be re-registered without its
// repo, so restore surfaces this as an actionable error and leaves the archived
// worktree intact for the user to salvage manually (#1028).
var ErrRepoGone = errors.New("origin repository is gone")

// ErrRelocateStateUnknown is joined into a MoveWorktree/RestoreWorktreeTo error
// when a git step on the relocate path was cut off by its own deadline, so the
// operation's effect was never established: SIGKILLed mid-move or mid-repair,
// git may have moved the bytes, updated part of its registration, or neither.
//
// It exists because bounding those calls (#1917) created an outcome they could
// not previously report. session/teardown.go's archive mode says so explicitly:
// an UNBOUNDED move either moves or answers with an error the daemon rolls the
// session back to Lost on, so it always returned stateKnown — "if the move is
// ever bounded, a tripped deadline must return stateUnknown here". Callers that
// finalize a teardown (clearing tmux refs and the worktree pointer, which is
// exactly what a retry needs to find intact) must check for this and skip
// finalization rather than treat the timeout as an ordinary failed move.
//
// Only ERRORS carry it. A deadline that trips on the fast path and is then
// recovered by the manual move + a repair that SUCCEEDS establishes the end
// state, and that returns nil.
var ErrRelocateStateUnknown = errors.New("the worktree relocation was cut off by a deadline: its on-disk and registration state is unestablished")

// Every git call on the relocate path below uses runGitLocalCommand, not
// runGitCommand. Archive and restore are session-TEARDOWN/lifecycle paths held
// under the daemon's per-session guards, and the runner contract in
// worktree_git.go requires such callers to be bounded (#1917): an unbounded
// call against a stalled filesystem leaves the session wedged forever in its
// optimistic Archiving/Restoring state instead of surfacing a timeout.

// worktreeMoveFast is the git-native fast path for relocating a worktree —
// `git worktree move`, which is atomic on a single filesystem and updates the
// two-way registration itself. It is a package var so tests can force the
// manual-move + `git worktree repair` fallback deterministically without a real
// second filesystem. Production never reassigns it.
var worktreeMoveFast = func(g *GitWorktree, src, dest string) error {
	_, err := g.runGitLocalCommand(g.repoPath, "worktree", "move", src, dest)
	return err
}

// relocationIdentityTimeout bounds every read-only identity check around a
// bounded worktree move. The filesystem itself may be stalled, so neither the
// pre-move snapshot nor a later recovery probe may wedge teardown.
var relocationIdentityTimeout = 2 * time.Second

func inspectRelocationPathIdentity(path string) (pathIdentity, error) {
	parent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(path), "relocation parent")
	if err != nil {
		return pathIdentity{}, err
	}
	defer parent.Close()
	identity, err := identityAt(parent, filepath.Base(path))
	if err != nil {
		return pathIdentity{}, fmt.Errorf("inspect relocation path %s: %w", path, err)
	}
	return identity, nil
}

// relocationPathIdentity is a test seam for filesystem metadata I/O. Production
// always uses inspectRelocationPathIdentity.
var relocationPathIdentity = inspectRelocationPathIdentity

// boundedRelocationPathIdentity runs metadata I/O behind a hard caller bound.
// Filesystem syscalls cannot be cancelled in-process, so a truly uninterruptible
// mount may retain one read-only goroutine for this failed lifecycle attempt;
// the buffered result still lets a late completion exit, while the daemon and
// the only durable session record remain responsive.
func boundedRelocationPathIdentity(path string) (pathIdentity, error) {
	type result struct {
		identity pathIdentity
		err      error
	}
	resultC := make(chan result, 1)
	go func() {
		identity, err := relocationPathIdentity(path)
		resultC <- result{identity: identity, err: err}
	}()

	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case observed := <-resultC:
		return observed.identity, observed.err
	case <-timer.C:
		return pathIdentity{}, fmt.Errorf(
			"timed out after %s while checking relocation path %s: %w",
			relocationIdentityTimeout, path, context.DeadlineExceeded,
		)
	}
}

// worktreeContainsSubmodules identifies the worktree shape that `git worktree
// move` explicitly does not support: a worktree with an INITIALIZED submodule.
// Checking before the move keeps the manual move + repair path a deliberate
// implementation choice instead of first manufacturing a Git failure for every
// archive of such a repository.
//
// "Declared" is not "initialized", and only the latter blocks the fast path. A
// worktree that merely records a submodule gitlink it never checked out (any
// clone without --recursive) still prints a line — `git submodule status` marks
// an UNINITIALIZED entry with a leading '-'. Treating every line as initialized
// sent those worktrees down the manual path even though `git worktree move`
// handles them natively (verified on git 2.43), trading an atomic rename for a
// byte-move plus a repair that can time out or leave a stale registration.
//
// The probe reads an initialized submodule's gitdir, so it inherits exactly the
// stall it is meant to precede: bounded, a hung mount surfaces a timeout the
// caller reports; unbounded, this new pre-check would have added a fresh way to
// wedge archive and restore.
var worktreeContainsSubmodules = func(g *GitWorktree, src string) (bool, error) {
	out, err := g.runGitLocalCommand(src, "submodule", "status")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		return true, nil
	}
	return false, nil
}

// worktreeRepair re-links a manually moved worktree's two-way registration
// (`git worktree repair`). A package var for the same test-seam reason as
// worktreeMoveFast: it lets a test force a repair failure AFTER a successful
// byte-move to prove the location is still committed. Production never
// reassigns it.
var worktreeRepair = func(g *GitWorktree, dest string) error {
	_, err := g.runGitLocalCommand(g.repoPath, "worktree", "repair", dest)
	return err
}

// worktreeRepairSubmodules re-points initialized submodules after a raw
// directory move. `git worktree repair` fixes the superproject's .git pointer,
// but submodule .git files can still contain relative gitdir paths computed
// from the old worktree location. `git submodule absorbgitdirs` rewrites those
// pointers without fetching or checking out new content; the foreach pass makes
// the repair explicit for initialized nested submodules on Git versions whose
// top-level absorb does not recurse.
var worktreeRepairSubmodules = func(g *GitWorktree, dest string) error {
	if _, err := g.runGitLocalCommand(dest, "submodule", "absorbgitdirs"); err != nil {
		return err
	}
	_, err := g.runGitLocalCommand(dest, "submodule", "foreach", "--recursive", "git submodule absorbgitdirs")
	return err
}

// Filesystem operation seams let tests force cross-device and cleanup-failure
// paths deterministically. Production never reassigns them.
var (
	renamePath                  = renamePathNoReplace
	removeDirectoryTree         = removeOpenedDirectory
	moveDirInspectClaimedSource = identityAt
	copyTreeBeforeSourceOpen    = func(string) error { return nil }
	copyTreeAfterSourceInspect  = func(string) error { return nil }
	copyTreeAfterSymlinkCreate  = func(string) error { return nil }
	copyTreeBeforeSymlinkStamp  = func(string) error { return nil }
	copyTreeAfterDestCreate     = func(string) error { return nil }
	moveDirBeforeDestParentOpen = func(string) error { return nil }
	moveDirBeforeDestCommit     = func(string) error { return nil }
	moveDirBeforeSourceCommit   = func(string) error { return nil }
	moveDirAfterDestCommit      = func(string) error { return nil }
	renamePathAfterCommit       = func(string) error { return nil }
	removeTreeBeforeEntryClaim  = func(*os.File, string) error { return nil }
)

// MoveWorktree relocates this worktree's directory to dest and keeps git's
// two-way worktree link consistent (the worktree's `.git` file and the repo's
// `.git/worktrees/<name>/gitdir`). It is the strict general move: unlike
// ArchiveWorktree, it never permits a known-absent destination entry.
//
// Uncommitted changes and the branch are preserved by construction — the
// working directory is moved verbatim, never re-checked-out. On success
// g.worktreePath / g.worktreeDir are updated to point at dest.
func (g *GitWorktree) MoveWorktree(dest string) error {
	return g.relocateWorktreeTo(dest, "move", nil)
}

// MoveWorktreeWithClaim carries a previously resolved source identity through a
// strict move.
func (g *GitWorktree) MoveWorktreeWithClaim(dest string, claim RelocationClaim) error {
	return g.relocateWorktreeTo(dest, "move", &claim)
}

// ArchiveWorktree is the one relocation role allowed to publish an incomplete
// copy. It retains the complete original tree and records every known-absent
// destination entry; MoveWorktree and restore keep the zero-value refusal.
func (g *GitWorktree) ArchiveWorktree(dest string) error {
	return g.relocateWorktreeTo(dest, "archive", nil)
}

// ArchiveWorktreeWithClaim is ArchiveWorktree with the teardown caller's
// point-in-time ownership carried through the pane-exit window.
func (g *GitWorktree) ArchiveWorktreeWithClaim(dest string, claim RelocationClaim) error {
	return g.relocateWorktreeTo(dest, "archive", &claim)
}

// RestoreWorktreeTo moves this (archived) worktree back to dest and re-registers
// it against the origin repo — the restore-side primitive (#1028). It first
// verifies the origin repo still exists (ErrRepoGone otherwise), because a
// worktree cannot be repaired/re-registered without its repository; the repair
// runs against wherever the repo now lives, so a repo that itself moved on disk
// since archiving is handled.
func (g *GitWorktree) RestoreWorktreeTo(dest string) error {
	claim, err := g.ClaimRelocationSource()
	if err != nil {
		return err
	}
	return g.RestoreWorktreeToWithClaim(dest, claim)
}

// RestoreWorktreeToWithClaim is the recovery-aware restore entrypoint. The
// caller resolved the archived source before deriving restore context, so carry
// that same ownership through the repo check and relocation rather than reading
// the lifecycle a second time.
func (g *GitWorktree) RestoreWorktreeToWithClaim(dest string, claim RelocationClaim) error {
	if err := g.ensureRepoPresent(); err != nil {
		if errors.Is(err, ErrRepoGone) {
			// The daemon's early guard is only a convenience; this is the
			// authoritative check at the worktree-use boundary. Materialize only a
			// non-destructive fence here; the daemon must persist it before it may
			// install cleanup authority and its durable generation.
			g.PreserveRelocationClaimAsUnresolved(claim)
		} else {
			g.PreserveRelocationClaimAsUnresolved(claim)
		}
		return err
	}
	return g.relocateWorktreeTo(dest, "restore", &claim)
}

// relocateWorktreeTo is the shared move engine behind archive, move, and
// restore. Fast path: `git worktree move`. Git explicitly refuses
// linked worktrees containing submodules, so that known shape goes directly to
// the manual move + repair path. Because the archive root ($AF_HOME) is
// frequently on a different filesystem than the repo, the fast path can also
// fail with EXDEV; any attempted fast-path failure uses the same fallback.
//
// The fallback moves the directory bytes (rename, or copy+remove across
// devices) and runs `git worktree repair`, which is purpose-built to fix a
// manually moved worktree. `git worktree move` validates and renames before
// touching its config, so on failure the source is normally left intact and the
// fallback is safe; the dest-already-moved check covers the rare partial-move
// case.
//
// Because every git step here is bounded, a step can now be SIGKILLed partway
// through instead of failing or finishing. Any error returned after a tripped
// deadline is joined with ErrRelocateStateUnknown so a teardown caller can tell
// "the move failed, roll back" from "we do not know what the move did" — the
// second must not finalize. A deadline that trips and is then RECOVERED (the
// fallback moves the bytes and the repair succeeds) establishes the end state
// and returns nil, so the marker never rides on a success.
// operation names what the CALLER is doing, because this engine cannot infer
// the stakes: archive explicitly permits a known absence and retains its source;
// move and restore refuse. Unknown future operations inherit refusal (#3066).
func (g *GitWorktree) relocateWorktreeTo(dest, operation string, requiredClaim *RelocationClaim) error {
	// A non-nil report marks an archive operation. Start with every prior retained
	// tree: restoring an incomplete archive cannot recreate omitted bytes, so a
	// later complete copy is not evidence that the old omissions were recovered.
	// Only an explicit destructive cleanup may retire that ownership record.
	var completedArchiveReport *ArchiveReport
	if operation == "archive" {
		report := g.GetArchiveReport()
		completedArchiveReport = &report
	}
	var sourceClaim RelocationClaim
	var claimErr error
	if requiredClaim == nil {
		sourceClaim, claimErr = g.ClaimRelocationSource()
	} else {
		sourceClaim = *requiredClaim
		claimErr = g.RevalidateRelocationClaim(sourceClaim)
	}
	if claimErr != nil {
		return claimErr
	}
	claimSettled := false
	defer func() {
		if !claimSettled {
			g.PreserveRelocationClaim(sourceClaim)
		}
	}()
	finishRelocation := func() {
		g.finishRelocationClaim(sourceClaim, dest, completedArchiveReport)
		claimSettled = true
	}
	src := sourceClaim.Path
	if g.externalWorktree {
		return fmt.Errorf("cannot relocate an in-place/external worktree at %s (it is user-owned)", src)
	}
	if src == "" {
		return fmt.Errorf("cannot relocate worktree: source path is empty")
	}
	if dest == "" {
		return fmt.Errorf("cannot relocate worktree: destination path is empty")
	}

	// deadlineTripped latches when any bounded step was cut off rather than
	// answering. It classifies later errors and prevents unbounded cleanup from
	// treating the same workspace as healthy.
	deadlineTripped := false
	noteDeadline := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) {
			deadlineTripped = true
			// The persisted lifecycle record is the latch. Creating it in the
			// timeout chokepoint makes absence impossible to misread as success.
			g.markRelocationStalled(&sourceClaim)
		}
		return err
	}
	// unknownIfCutOff joins the state marker onto a failure that followed a
	// tripped deadline OR happened while two relocation candidates remain. The
	// latter must survive every retry error until one location is established.
	unknownIfCutOff := func(err error) error {
		if !deadlineTripped && !g.hasRelocationRecoveryRecord() {
			return err
		}
		return errors.Join(err, ErrRelocateStateUnknown)
	}
	repairDestination := func() error {
		// Resolution is a point-in-time claim. Revalidate immediately before each
		// registration repair attempt, including a retry whose bytes were already
		// established at dest.
		if err := g.RevalidateRelocationClaim(sourceClaim); err != nil {
			return err
		}
		if repairErr := noteDeadline(worktreeRepair(g, dest)); repairErr != nil {
			if !deadlineTripped {
				g.PreserveRelocationClaim(sourceClaim)
			}
			return unknownIfCutOff(fmt.Errorf(
				"worktree bytes were established at %s but git registration repair did not complete: %w",
				dest, repairErr,
			))
		}
		// Registration repair is one pathname use; submodule repair is another.
		// A same-uid process can replace dest while the first command runs, so its
		// earlier identity proof cannot authorize this second command.
		if err := g.RevalidateRelocationClaim(sourceClaim); err != nil {
			return err
		}
		if submoduleErr := noteDeadline(worktreeRepairSubmodules(g, dest)); submoduleErr != nil {
			if deadlineTripped {
				return unknownIfCutOff(fmt.Errorf(
					"moved worktree to %s but submodule gitdir repair was cut off: %w", dest, submoduleErr,
				))
			}
			log.WarningLog.Printf(
				"submodule gitdir repair failed after moving worktree to %s; "+
					"run `%s` "+
					"(or `%s`) "+
					"to fix submodule status; continuing because the worktree move "+
					"and registration repair already succeeded: %v",
				dest,
				shellsuggest.Command("git", "-C", dest, "submodule", "absorbgitdirs"),
				shellsuggest.Command("git", "-C", dest, "submodule", "update", "--init", "--recursive"),
				submoduleErr,
			)
		}
		// Settlement is the final identity boundary. Revalidate and release the
		// consumed recovery claim together so a replacement installed during the
		// submodule command remains unresolved rather than becoming the recorded
		// worktree.
		if err := g.SettleRelocationClaim(sourceClaim); err != nil {
			return err
		}
		claimSettled = true
		return nil
	}

	if src == dest {
		if err := repairDestination(); err != nil {
			return err
		}
		return nil
	}
	if pathExists(dest) {
		return unknownIfCutOff(fmt.Errorf("cannot relocate worktree: destination %s already exists", dest))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return unknownIfCutOff(fmt.Errorf("failed to create destination parent directory for %s: %w", dest, err))
	}
	// Every refusal is behind us and no bytes have moved yet: reap the writers
	// that would otherwise be left pointed at a vacated pathname (#3391).
	vacated := reapRelocationSourceWriters(src, dest)

	useFallback, inspectErr := worktreeContainsSubmodules(g, src)
	if inspectErr != nil {
		// The probe mutates nothing, so a timeout here leaves no partial state of
		// its own — but it still latches, because a probe that could not read the
		// worktree means the steps below are running against the same stalled
		// filesystem and their own outcome is that much less certain.
		noteDeadline(inspectErr)
		if deadlineTripped {
			return unknownIfCutOff(fmt.Errorf(
				"refusing to continue relocating worktree %s after the submodule inspection timed out: %w",
				src, inspectErr,
			))
		}
		log.InfoLog.Printf(
			"could not inspect worktree %s for submodules (%v); trying git worktree move",
			src, inspectErr,
		)
	}
	if !useFallback {
		// The record was consumed at resolution. Revalidate its ephemeral claim
		// at the fast move's use boundary, then recreate a two-candidate record if
		// the bounded command stops answering.
		if err := g.RevalidateRelocationClaim(sourceClaim); err != nil {
			return err
		}
		if err := noteDeadline(worktreeMoveFast(g, src, dest)); err != nil {
			if deadlineTripped {
				// Record BOTH names before returning. Destination is the primary
				// candidate because git may have completed the rename; source is the
				// durable alternate. Neither authorizes destruction until a retry
				// matches the captured identity under a bound.
				g.beginRelocationRecovery(dest, src, sourceClaim.identity)
				return unknownIfCutOff(fmt.Errorf(
					"git worktree move was cut off for %s -> %s; retained both paths for bounded recovery and refused a second move against their unknown state: %w",
					src, dest, err,
				))
			}
			// An ANSWERED error can still arrive after git renamed the directory but
			// failed while updating registration. Materialize the same two-candidate
			// state as a timeout, resolve it immediately under the identity bound, and
			// only then choose the manual repair path. Revalidating the now-missing src
			// first would discard dest, which may already hold the user's work.
			attemptedSource := src
			g.beginRelocationRecovery(dest, attemptedSource, sourceClaim.identity)
			resolvedClaim, resolveErr := g.ClaimRelocationSource()
			if resolveErr != nil {
				return errors.Join(fmt.Errorf(
					"git worktree move answered with an error and its partial effect could not be resolved for %s -> %s: %w",
					attemptedSource, dest, resolveErr,
				), ErrRelocateStateUnknown)
			}
			sourceClaim = resolvedClaim
			src = resolvedClaim.Path
			// The fallback is the designed recovery for cross-device moves and
			// other fast-path limitations. Record why it was selected without
			// reporting a failed archive; actual fallback/repair failures return
			// below and are surfaced by the caller.
			log.InfoLog.Printf(
				"git worktree move unavailable for %s -> %s (%v); resolved its partial effect at %s and is using manual move + repair",
				attemptedSource, dest, err, src,
			)
			useFallback = true
		}
	}
	if useFallback && src == dest {
		// An answered fast-path error may have committed the rename before
		// reporting a registration failure. Candidate resolution above selected
		// dest, so only the unfinished repairs remain; do not manufacture an
		// identical two-path recovery record or attempt a second move.
		if err := repairDestination(); err != nil {
			return err
		}
		reportRelocationResidue(vacated)
		return nil
	}
	if useFallback {
		// Source selection consumed the record. The fallback may proceed only if
		// that point-in-time claim still names the same directory now.
		if err := g.RevalidateRelocationClaim(sourceClaim); err != nil {
			return err
		}
		// Candidate resolution above tells us whether the fast path already moved
		// the bytes. Move only when the selected source is still distinct from dest;
		// pathname existence alone cannot distinguish the worktree from a raced-in
		// replacement.
		var sourceCleanupErr error
		var sourceCleanupPath string
		var sourceCleanupPathVerified bool
		movedIdentity := sourceClaim.identity
		publicationCheckpointed := false
		checkpointPublication := func(
			report ArchiveReport, identity pathIdentity, publish func() error,
		) error {
			var committedReport *ArchiveReport
			if completedArchiveReport != nil {
				combined := completedArchiveReport.append(report)
				committedReport = &combined
			}
			if err := g.checkpointRelocationPublication(
				sourceClaim, dest, src, identity, committedReport, publish,
			); err != nil {
				return err
			}
			completedArchiveReport = committedReport
			publicationCheckpointed = true
			return nil
		}
		if src != dest {
			var report ArchiveReport
			if mErr := moveDirCrossDeviceRecordingIdentity(
				src, dest, operation, unreadablePolicyForOperation(operation), &movedIdentity, &report,
				checkpointPublication,
			); mErr != nil {
				var copiedErr *copiedWorktreeSourceCleanupError
				if !errors.As(mErr, &copiedErr) {
					return unknownIfCutOff(fmt.Errorf("failed to move worktree %s -> %s: %w", src, dest, mErr))
				}
				sourceCleanupErr = mErr
				sourceCleanupPath = copiedErr.src
				sourceCleanupPathVerified = copiedErr.cleanupPathVerified
			}
			if !report.Empty() {
				// The publication callback installed this report together with the
				// destination path and unresolved ownership before it released the
				// persistence lock. Seeing it here therefore means the durable state is
				// already safe for registration repair or a process exit.
				if completedArchiveReport == nil {
					return fmt.Errorf("archive publication returned a report without committing it")
				}
			}
		}
		// The bytes are now at dest but registration repair is still outstanding.
		// Materialize that exact destination identity before repair, then consume it
		// into a new active claim. A checkpoint or error at every point therefore
		// retains the only pathname which owns the user's files.
		if !publicationCheckpointed {
			g.beginRelocationRecovery(dest, src, movedIdentity)
		}
		committedClaim, committedErr := g.ClaimRelocationSource()
		if committedErr != nil {
			return errors.Join(fmt.Errorf(
				"moved worktree bytes to %s but could not claim the committed destination for registration repair: %w",
				dest, committedErr,
			), ErrRelocateStateUnknown)
		}
		sourceClaim = committedClaim
		if rErr := repairDestination(); rErr != nil {
			if sourceCleanupErr != nil {
				return errors.Join(rErr, fmt.Errorf(
					"copied worktree to %s but failed to remove original %s: %w",
					dest, sourceCleanupPath, sourceCleanupErr,
				))
			}
			return rErr
		}
		if sourceCleanupErr != nil {
			// Copy AND git registration both succeeded: the worktree is valid,
			// registered, and usable at dest. Removing the leftover source dir is
			// the only step that failed — a disk-reclamation nuisance, not a move
			// failure. Returning an error here is actively harmful (#2011): it
			// drives the daemon's archive-rollback / restore-retry logic even
			// though a valid worktree already exists at dest, and the retry picks a
			// fresh collision-suffixed dest, copies + registers a SECOND worktree,
			// orphaning the first and corrupting `git worktree list` and branch
			// exclusivity. The old "remove the original manually" advice is worse
			// than useless: instance state may still point at src, so following it
			// breaks recovery. Warn (so the leftover disk stays visible and
			// reclaimable) and return nil.
			if sourceCleanupPathVerified {
				log.WarningLog.Printf(
					"worktree copied and registered at %s, but failed to remove the leftover source directory %s; "+
						"the worktree is valid and usable at %s — the leftover is only reclaimable disk, "+
						"remove it by hand with `%s`: %v",
					dest, sourceCleanupPath, dest,
					shellsuggest.Command("rm", "-rf", sourceCleanupPath),
					sourceCleanupErr,
				)
			} else {
				log.WarningLog.Printf(
					"worktree copied and registered at %s, but source cleanup could not determine the original tree's current pathname; "+
						"the worktree is valid and usable at %s, but do not delete the stale quarantine name %s because it now identifies different data: %v",
					dest, dest, sourceCleanupPath, sourceCleanupErr,
				)
			}
		}
		reportRelocationResidue(vacated)
		return nil
	}

	// Fast path succeeded: git moved the bytes and updated the registration.
	finishRelocation()
	reportRelocationResidue(vacated)
	return nil
}

// ensureRepoPresent reports ErrRepoGone when the origin repo is missing or no
// longer a git repository. Used by RestoreWorktreeTo so the caller can surface
// the repo-gone case distinctly (leave the archive intact) rather than as a
// generic move failure.
func (g *GitWorktree) ensureRepoPresent() error {
	return boundedRepoGoneOriginProbe(g)
}

// moveDirCrossDevice moves src to dest, falling back to a copy+remove when the
// two paths straddle a filesystem boundary (os.Rename returns EXDEV) — the
// common case when the archive root lives on a different device than the repo.
// The copy preserves file contents, modes, and symlinks, so uncommitted changes
// survive verbatim.
// operation names the caller's action, purely for error text. It is stamped onto
// an unreadableSourceError BEFORE that error is wrapped: fmt.Errorf formats and
// CACHES the inner error's text at construction, so stamping after the wrap
// changes the struct and not the message the user sees (#3087 review).
func moveDirCrossDevice(src, dest, operation string) error {
	return moveDirCrossDeviceRecordingIdentity(src, dest, operation, refuseUnreadable, nil, nil, nil)
}

type relocationPublicationCheckpoint func(ArchiveReport, pathIdentity, func() error) error

// moveDirCrossDeviceRecordingIdentity is the relocation path's variant. The
// caller initializes committedIdentity with the claimed source identity. A
// same-filesystem rename preserves it; a verified cross-device publication
// replaces it with the copied root identity at the exact no-replace commit.
func moveDirCrossDeviceRecordingIdentity(
	src, dest, operation string,
	policy unreadablePolicy,
	committedIdentity *pathIdentity,
	archiveReport *ArchiveReport,
	checkpoint relocationPublicationCheckpoint,
) (returnErr error) {
	renamePublish := func() error { return renamePath(src, dest) }
	var renameErr error
	if checkpoint != nil {
		if committedIdentity == nil {
			return fmt.Errorf("relocation publication checkpoint requires a committed identity")
		}
		renameErr = checkpoint(ArchiveReport{}, *committedIdentity, renamePublish)
	} else {
		renameErr = renamePublish()
	}
	if renameErr == nil {
		return nil
	} else if !errors.Is(renameErr, syscall.EXDEV) {
		return renameErr
	}
	// Cross-device: copy into an unguessable sibling, atomically claim the
	// verified source endpoint, then atomically publish the copied directory at
	// dest without replacing anything. These two renames are the commit boundary:
	// until both identities match, the source is restored and never deleted.
	stagingPath, err := privateMovePath(dest, "copy")
	if err != nil {
		return err
	}
	copied, err := copyTreeWithPolicy(src, stagingPath, policy)
	if err != nil {
		// Stamp first, wrap second. The order is the whole point.
		var unreadable *unreadableSourceError
		if errors.As(err, &unreadable) {
			unreadable.operation = operation
		}
		return fmt.Errorf("failed to copy worktree into private staging directory %s: %w", stagingPath, err)
	}
	defer copied.close()
	stagingName := filepath.Base(stagingPath)
	published := false
	defer func() {
		if published {
			return
		}
		stagingManifest := destinationCleanupManifest(copied.root)
		if cleanupErr := removeDirectoryTree(
			copied.destinationParent, stagingName, stagingPath, copied.destination, &stagingManifest,
		); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("failed to clean private staging tree %s: %w", stagingPath, cleanupErr))
		}
	}()

	sourceParentPath := filepath.Dir(src)
	sourceParent, _, err := openDirectoryPathFollowingLinks(sourceParentPath, "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	sourceParentIdentity, err := identityFromFile(sourceParent)
	if err != nil {
		return err
	}
	sourceName := filepath.Base(src)
	quarantinePath, err := privateMovePath(src, "source")
	if err != nil {
		return err
	}
	if err := moveDirBeforeSourceCommit(src); err != nil {
		return err
	}
	quarantineName := filepath.Base(quarantinePath)
	if err := renameAtNoReplace(int(sourceParent.Fd()), sourceName, int(sourceParent.Fd()), quarantineName); err != nil {
		return fmt.Errorf("failed to atomically secure source directory %s before cleanup: %w", src, err)
	}
	quarantinedIdentity, err := moveDirInspectClaimedSource(sourceParent, quarantineName)
	if err != nil {
		// Restore only what still identifies the source this process opened. A
		// racer can strand the claimed entry and drop a replacement at the
		// quarantine name inside this window; an unchecked rename would publish
		// that replacement at src and report it as the restored source, while
		// the real tree stayed stranded under the racer's name.
		if restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName, copied.source); restoreErr != nil {
			return fmt.Errorf("failed to inspect secured source %s (%v) and could not restore it to %s: %w", quarantinePath, err, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, copied.sourceIdentity,
		); pathErr != nil {
			return errors.Join(fmt.Errorf("failed to inspect secured source %s: %w", quarantinePath, err), pathErr)
		}
		return fmt.Errorf("failed to inspect secured source %s; restored it to %s: %w", quarantinePath, src, err)
	}
	if !copied.sourceIdentity.same(quarantinedIdentity) {
		restoreErr := restoreClaimedSource(sourceParent, quarantineName, sourceName)
		if restoreErr != nil {
			return fmt.Errorf("source directory changed while it was copied; replacement was preserved at %s but could not be restored to %s: %w", quarantinePath, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, quarantinedIdentity,
		); pathErr != nil {
			return errors.Join(errors.New("source directory changed while it was copied"), pathErr)
		}
		return fmt.Errorf("source directory changed while it was copied; restored the replacement at %s and refused cleanup", src)
	}
	restoreSource := func(cause error) error {
		restoreErr := restoreSecuredSource(sourceParent, quarantineName, sourceName, copied.source)
		if restoreErr != nil {
			return fmt.Errorf("%v; secured source at %s could not be restored to %s: %w", cause, quarantinePath, src, restoreErr)
		}
		if pathErr := validateNamedPathIdentity(
			sourceParentPath, sourceName, "source", sourceParentIdentity, copied.sourceIdentity,
		); pathErr != nil {
			return errors.Join(cause, pathErr)
		}
		return cause
	}

	destinationParentPath := filepath.Dir(dest)
	if err := moveDirBeforeDestParentOpen(destinationParentPath); err != nil {
		return restoreSource(err)
	}
	currentDestinationParent, _, err := openDirectoryPathFollowingLinks(destinationParentPath, "destination parent")
	if err != nil {
		return restoreSource(err)
	}
	currentDestinationParentIdentity, err := identityFromFile(currentDestinationParent)
	currentDestinationParent.Close()
	if err != nil || !copied.destinationParentIdentity.same(currentDestinationParentIdentity) {
		if err == nil {
			err = fmt.Errorf("destination parent changed while the worktree was copied")
		}
		return restoreSource(err)
	}
	if err := moveDirBeforeDestCommit(stagingPath); err != nil {
		return restoreSource(err)
	}
	if err := copied.validateSource(quarantinePath); err != nil {
		return restoreSource(fmt.Errorf("source tree changed after copy: %w", err))
	}
	if err := copied.validateDestination(stagingPath); err != nil {
		return restoreSource(fmt.Errorf("destination tree changed after copy: %w", err))
	}
	var report ArchiveReport
	if len(copied.skipped) > 0 {
		if archiveReport == nil {
			return restoreSource(fmt.Errorf("copier skipped unreadable files without an archive report channel"))
		}
		report = ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
			newArchiveRetainedTree(quarantinePath, quarantinedIdentity, copied.skipped),
		}}
	}
	publishDestination := func() error {
		if err := renameAtNoReplace(
			int(copied.destinationParent.Fd()), stagingName,
			int(copied.destinationParent.Fd()), filepath.Base(dest),
		); err != nil {
			return fmt.Errorf("failed to atomically commit copied worktree at %s without replacement: %w", dest, err)
		}
		published = true
		return errors.Join(moveDirAfterDestCommit(dest), validatePublishedDestination(dest, copied))
	}
	var commitErr error
	if checkpoint != nil {
		commitErr = checkpoint(report, copied.destinationIdentity, publishDestination)
	} else {
		commitErr = publishDestination()
	}
	if commitErr != nil {
		if published {
			destinationManifest := destinationCleanupManifest(copied.root)
			cleanupErr := removeDirectoryTree(
				copied.destinationParent, filepath.Base(dest), dest, copied.destination, &destinationManifest,
			)
			if cleanupErr != nil {
				commitErr = errors.Join(commitErr, fmt.Errorf("failed to remove unverified destination %s: %w", dest, cleanupErr))
			}
		}
		return restoreSource(commitErr)
	}
	if committedIdentity != nil {
		*committedIdentity = copied.destinationIdentity
	}
	if !report.Empty() {
		// Keep the COMPLETE secured source rather than deleting bytes the archive
		// never copied. This is intentionally archive-only: the report pointer is
		// supplied only by relocateWorktreeTo's explicit archive role. The hidden
		// source is inert (git registration now points at dest) but recoverable, and
		// its exact location travels in the durable session report.
		*archiveReport = report
		return nil
	}

	if err := removeDirectoryTree(sourceParent, quarantineName, quarantinePath, copied.source, &copied.root); err != nil {
		var unverified *unverifiedCleanupPathError
		cleanupPathVerified := !errors.As(err, &unverified)
		if pathErr := validateDirectoryPathIdentity(sourceParentPath, "source", sourceParentIdentity); pathErr != nil {
			err = errors.Join(err, pathErr)
			cleanupPathVerified = false
		}
		return &copiedWorktreeSourceCleanupError{
			src:                 quarantinePath,
			dest:                dest,
			err:                 err,
			cleanupPathVerified: cleanupPathVerified,
		}
	}
	return nil
}

type copiedWorktreeSourceCleanupError struct {
	src                 string
	dest                string
	err                 error
	cleanupPathVerified bool
}

func (e *copiedWorktreeSourceCleanupError) Error() string {
	if !e.cleanupPathVerified {
		return fmt.Sprintf("copied worktree to %s but could not determine the original source's current pathname near %s: %v", e.dest, e.src, e.err)
	}
	return fmt.Sprintf("copied worktree to %s but failed to remove original %s: %v", e.dest, e.src, e.err)
}

func (e *copiedWorktreeSourceCleanupError) Unwrap() error {
	return e.err
}

func openDirectoryPath(path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

// openDirectoryPathFollowingLinks is used only for an already-configured
// destination parent. Users may intentionally make worktree_root a symlink to
// another filesystem; O_DIRECTORY and O_NONBLOCK still reject a raced-in FIFO,
// while all writes remain anchored to the returned directory descriptor.
func openDirectoryPathFollowingLinks(path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

func openDirectoryAt(parent *os.File, name, path, role string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(
		int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: failed to open %s directory %s safely: %w", role, path, err)
	}
	return openedDirectory(fd, path, role)
}

func openedDirectory(fd int, path, role string) (*os.File, os.FileInfo, error) {
	dir := os.NewFile(uintptr(fd), path)
	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	if !info.IsDir() {
		_ = dir.Close()
		return nil, nil, fmt.Errorf("cannot move worktree across filesystems: %s path %s is not a directory", role, path)
	}
	return dir, info, nil
}

func readLinkAt(parent *os.File, name, path string) (string, error) {
	for size := 256; size <= 64*1024; size *= 2 {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(int(parent.Fd()), name, buffer)
		if err != nil {
			return "", fmt.Errorf("cannot move worktree across filesystems: failed to read source symlink %s safely: %w", path, err)
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
	}
	return "", fmt.Errorf("cannot move worktree across filesystems: source symlink %s target is too long", path)
}

func unsupportedSourceTypeError(path string, mode uint32) error {
	return fmt.Errorf("cannot move worktree across filesystems: unsupported file type at %s (mode %#o)", path, mode&unix.S_IFMT)
}

func privateMovePath(path, purpose string) (string, error) {
	name, err := privateMoveName(purpose)
	if err != nil {
		return "", fmt.Errorf("generate private %s path beside %s: %w", purpose, path, err)
	}
	return filepath.Join(filepath.Dir(path), name), nil
}

func privateMoveName(purpose string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(".af-%s-%s", purpose, hex.EncodeToString(random[:])), nil
}

func restoreClaimedSource(parent *os.File, securedName, sourceName string) error {
	return renameAtNoReplace(int(parent.Fd()), securedName, int(parent.Fd()), sourceName)
}

func restoreSecuredSource(parent *os.File, securedName, sourceName string, source *os.File) error {
	expected, err := identityFromFile(source)
	if err != nil {
		return err
	}
	current, err := identityAt(parent, securedName)
	if err != nil || !expected.same(current) {
		return fmt.Errorf("secured source name no longer identifies the opened source")
	}
	if err := restoreClaimedSource(parent, securedName, sourceName); err != nil {
		return err
	}
	restored, err := identityAt(parent, sourceName)
	if err != nil || !expected.same(restored) {
		return fmt.Errorf("restored source name does not identify the opened source")
	}
	return nil
}

// pathExists reports whether p exists (best-effort: a stat error other than
// not-exist is treated as "exists" so we never clobber an unreadable path).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil || !os.IsNotExist(err)
}

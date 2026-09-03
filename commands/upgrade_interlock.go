package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/sachiniyer/agent-factory/log"
)

// The two-installer interlock (#2212).
//
// Two mechanisms replace the same on-disk executable:
//
//  1. IN PLACE — `af upgrade` and launch-time auto-update. AtomicWriteFile over
//     the resolved executable, then restart the daemon. No journal, no
//     probation, no rollback.
//  2. TRANSACTIONAL — the daemon's activation path (internal/upgradetxn): an
//     fsynced journal, a preserved previous binary, probation, validated
//     activation, and guarded rollback.
//
// An in-place swap that lands mid-transaction writes the canonical path with no
// awareness of the transaction, destroying the rollback that transaction exists
// to guarantee — the failure #2212 calls the worst on the whole upgrade path,
// and strictly worse than not having a daemon-owned upgrade at all.
//
// Both mechanisms ship today: the daemon's update driver creates real
// transactions (daemon/update_driver.go → triggerUpgradeActivation →
// upgradetxn.Prepare) behind the explicit AGENT_FACTORY_DAEMON_UPGRADE opt-in,
// and the in-place installers remain the default. This file is the floor that
// coexistence cannot be safe without: the in-place writer never clobbers a live
// transaction. On a box that never opts in, no transaction exists and this
// resolves to "no journal → proceed" — pinned by a test.

// ignoreActiveUpgradeFlag is the `af upgrade` escape hatch. Named for what it
// ignores rather than a bare --force, so the thing being overridden is visible
// in the command the user types.
const ignoreActiveUpgradeFlag = "ignore-active-upgrade"

// allowRejectedFlag overrides the rejected-candidate ledger on the manual path.
const allowRejectedFlag = "allow-rejected"

// upgradeIgnoreActiveUpgrade is that flag's value. A package var, like
// upgradeAllowDowngrade, so adding it churns no runUpgrade call site.
var upgradeIgnoreActiveUpgrade bool

// loadUpgradeJournal is the seam over the real journal loader. A journal that
// upgradetxn.Load accepts can only be produced by a real Prepare — it is
// validated against the preserved binaries, their digests, and the recovery lock
// on disk — so hand-forging one per phase is not possible. The seam lets the
// POLICY below (which phases block, what unreadable evidence means) be tested
// exhaustively, while one test drives a genuinely Prepare'd transaction through
// the production path to prove the seam is wired to the real loader.
var loadUpgradeJournal = func(home string) (upgradetxn.Journal, error) {
	txn, err := upgradetxn.Load(home)
	if err != nil {
		return upgradetxn.Journal{}, err
	}
	return txn.Journal(), nil
}

// activeUpgrade describes a daemon upgrade transaction that currently owns this
// home's executable. Nil means an in-place swap may proceed.
type activeUpgrade struct {
	ID        string
	ToVersion string
	Phase     string
	// artifact is set only for a transaction discovered beside the executable
	// rather than through this home's journal: its home, version, and phase are
	// unknown, but the staged binary proves it exists and names it for a user
	// who needs to clear a leftover.
	artifact string
}

// blockedInPlaceInstallError is returned when an in-place swap is refused. It
// names the override, because a refusal a user cannot get past is not a
// safeguard — it is a brick.
type blockedInPlaceInstallError struct {
	active *activeUpgrade
	// flag is the command-line escape hatch, empty on paths that have none
	// (launch-time auto-update, which skips silently rather than refusing).
	flag string
}

func (e *blockedInPlaceInstallError) Error() string {
	msg := fmt.Sprintf(
		"a daemon upgrade to %s is in progress (transaction %s, phase %s); installing over it would destroy the rollback that upgrade depends on",
		e.active.ToVersion, e.active.ID, e.active.Phase,
	)
	if e.active.artifact != "" {
		msg = fmt.Sprintf(
			"a daemon upgrade is staging over this executable (transaction %s, preserved binary %s); it belongs to another agent-factory home, and installing over it would destroy the rollback that upgrade depends on",
			e.active.ID, e.active.artifact,
		)
	}
	if e.flag == "" {
		return msg
	}
	return msg + fmt.Sprintf(". Wait for it to finish, or re-run with %s to install anyway", e.flag)
}

// activeUpgradeOwningExecutable reports the transaction that currently owns this
// home's executable, or nil when an in-place swap may proceed.
//
// The polarity is deliberate and is the whole design of this function: it blocks
// ONLY on a positive, readable, non-terminal transaction. Everything else
// proceeds.
//
//   - No home, no journal, or ErrNoActiveTransaction — proceed. This is every
//     box that has not opted into daemon-owned activation
//     (AGENT_FACTORY_DAEMON_UPGRADE).
//   - A journal in a TERMINAL phase (committed, rolled_back, aborted) — proceed.
//     Those phases are cleanup only; the activation is decided and there is no
//     rollback left to corrupt.
//   - A journal that cannot be read or validated — proceed, loudly. This is the
//     case worth arguing about, so: refusing on unreadable evidence would let one
//     corrupt file disable `af upgrade` permanently, and `af doctor` has no
//     upgrade-transaction repair to send the user to. That is an unoverridable
//     block produced by an inference, which is the shape this repo has been bitten
//     by before. It is also the empirically wrong guess: a live transaction writes
//     a valid fsynced journal, so a corrupt one much more often means a broken
//     install that needs `af upgrade` to work than a live actor to protect.
func activeUpgradeOwningExecutable() *activeUpgrade {
	active, _ := activeUpgradeOwningExecutableWithID()
	return active
}

// activeUpgradeOwningExecutableWithID also reports THIS home's transaction id
// when one is readable, even in the cases the policy above deliberately lets
// through. The foreign-artifact scan needs it: a transaction of ours that has
// committed but not yet cleaned still has its preserved binary on disk, and
// blocking on that would contradict the decision this function just made.
func activeUpgradeOwningExecutableWithID() (*activeUpgrade, string) {
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("upgrade interlock: cannot resolve the agent-factory home; proceeding: %v", err)
		return nil, ""
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return nil, ""
	}
	journal, err := loadUpgradeJournal(home)
	if errors.Is(err, upgradetxn.ErrNoActiveTransaction) {
		return nil, ""
	}
	if err != nil {
		log.WarningLog.Printf("upgrade interlock: cannot read the daemon upgrade journal; proceeding with the in-place install: %v", err)
		return nil, ""
	}
	if isTerminalUpgradePhase(journal.Phase) {
		// Decided, cleanup pending. The id is reported so the artifact scan does
		// not re-block on this same transaction's not-yet-removed binary.
		return nil, journal.ID
	}
	// A valid journal is not proof there is still a rollback to protect.
	// upgradetxn.Load validates the recorded paths, digests, and lock metadata —
	// not the artifact contents — so a half-cleaned or failed transaction can
	// leave a loadable journal whose preserved previous binary is gone. Blocking
	// then protects nothing and only stands between the user and a working
	// `af upgrade`, which is the one thing they have left. Downgrade to "proceed"
	// only on a POSITIVE observation that the artifact is absent; an inconclusive
	// stat keeps the block, which is overridable.
	if _, err := os.Stat(journal.PreviousBinaryPath); errors.Is(err, os.ErrNotExist) {
		log.WarningLog.Printf(
			"upgrade interlock: daemon upgrade transaction %s (phase %s) has no preserved previous binary at %s; its rollback is already impossible, so the in-place install proceeds",
			journal.ID, journal.Phase, journal.PreviousBinaryPath,
		)
		return nil, journal.ID
	}
	return &activeUpgrade{
		ID:        journal.ID,
		ToVersion: journal.ToVersion,
		Phase:     string(journal.Phase),
	}, journal.ID
}

// isTerminalUpgradePhase reports whether the transaction has already decided its
// outcome, leaving only cleanup. rollback_failed is deliberately NOT terminal:
// it is the circuit-breaker state that retains every recovery artifact, so it is
// exactly when an unwitting overwrite does the most damage.
func isTerminalUpgradePhase(phase upgradetxn.Phase) bool {
	switch phase {
	case upgradetxn.PhaseCommitted, upgradetxn.PhaseRolledBack, upgradetxn.PhaseAborted:
		return true
	default:
		return false
	}
}

// foreignUpgradeStagingOver reports a transaction staging over this exact
// executable regardless of which AF home owns it, or nil when none is.
//
// One `af` binary can serve many AF homes (AGENT_FACTORY_HOME), but a
// transaction is home-scoped while the executable is not. So
// `AGENT_FACTORY_HOME=/tmp/other af upgrade` would look in /tmp/other, find no
// journal, and happily rename over the very binary the default home's
// transaction is preserving — destroying a rollback it never knew existed.
// There is no registry of AF homes to consult, but there does not need to be:
// upgradetxn stages its preserved and candidate binaries NEXT TO the executable
// (binaryArtifactPaths), so the executable's own directory is the one place
// every home's transaction is visible.
//
// Read with ReadDir and a literal prefix rather than filepath.Glob: an
// executable whose name contains a glob metacharacter would otherwise silently
// match the wrong set, and this decides whether to overwrite a binary.
func foreignUpgradeStagingOver(resolvedPath, ownID string) *activeUpgrade {
	dir := filepath.Dir(resolvedPath)
	prefix := "." + filepath.Base(resolvedPath) + ".af-upgrade-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Cannot enumerate: inconclusive, and inconclusive never blocks.
		return nil
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".previous") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".previous")
		if id == "" || id == ownID {
			// Ours, and the journal policy above already decided this home may
			// proceed — a committed transaction whose Cleanup has not yet removed
			// the preserved binary must not re-block the install it just allowed.
			continue
		}
		return &activeUpgrade{ID: id, artifact: filepath.Join(dir, name)}
	}
	return nil
}

// writeExecutableInPlace is the ONE guarded in-place binary swap. Both installers
// go through it, so the interlock cannot be bypassed by adding a call site — the
// entrypoint checks elsewhere exist to give a better message and to skip a
// pointless download, not to be the guard.
//
// override is the caller's explicit "install anyway"; it is honoured, and logged,
// because an unoverridable auto-upgrade safeguard is its own hazard.
func writeExecutableInPlace(resolvedPath string, binary []byte, override bool, flag string) error {
	return writeExecutableInPlaceAllowing(resolvedPath, binary, override, flag, false)
}

// writeExecutableInPlaceAllowing is the guarded swap with the REJECTED-CANDIDATE
// override made explicit and separate from the interlock's.
//
// Separate on purpose (#3043). Folding it into `override` would make
// --ignore-active-upgrade silently also bypass the rejected-candidate ledger — two
// unrelated safeguards behind one flag, and the kind of conflation that looks like a
// one-line simplification later. A caller must say which one it means.
func writeExecutableInPlaceAllowing(
	resolvedPath string, binary []byte, override bool, flag string, allowRejected bool,
) error {
	return writeExecutableInPlaceWaiting(resolvedPath, binary, override, flag, true, allowRejected)
}

// writeExecutableInPlaceWaiting is the guarded swap with the lock-wait policy
// made explicit. mayWait is false for the unattended launch updater, which runs
// in front of a TUI that has not opened yet: a daemon publishing a transaction
// holds the preparation lock for as long as it takes to copy and fsync a
// preserved binary, and stalling a launch behind that is the one thing this path
// may never do. `af upgrade` was asked for explicitly and waits.
func writeExecutableInPlaceWaiting(
	resolvedPath string, binary []byte, override bool, flag string, mayWait bool, allowRejected bool,
) error {
	swap := func() error {
		// The rejected-candidate ledger is consulted HERE, inside the lock that
		// serialises the swap, and this read is the one that decides (#3043).
		//
		// `af upgrade` also checks at its entrypoint, and that check stays — it gives
		// a better message and skips a pointless download. But it cannot be the
		// guard: it runs before this lock, so a daemon transaction that rolls back
		// and records a rejection in between is invisible to a check that already
		// passed, and the disqualified bytes land underneath it. That is the same
		// check-then-act window the interlock comment below describes for the
		// journal, and the same one #2859 closed by taking the snapshot inside
		// whichever lock serialises the swap. A re-check after the fact would only
		// move the window; the read has to happen where the mutation is serialised.
		if !allowRejected {
			rejected, entry, err := upgradetxn.CandidateRejected(resolvedPath, binary)
			if err != nil {
				// Fail CLOSED, matching CandidateRejected's own contract — "I could
				// not tell" is not "it is fine" — and both existing callers.
				return fmt.Errorf(
					"cannot read the rejected-candidate ledger, so this release is not safe to install (use --%s to install anyway): %w",
					allowRejectedFlag, err)
			}
			if rejected {
				return fmt.Errorf(
					"this release (%s) is byte-for-byte the build this machine rolled back at %s (%s); it failed validation here. Publish a corrected build, or pass --%s to install it anyway",
					entry.Version, entry.RejectedAt.Format(time.RFC3339), entry.Reason, allowRejectedFlag)
			}
		}
		active, ownID := activeUpgradeOwningExecutableWithID()
		if active == nil {
			// Nothing blocking in THIS home. The executable is shared across
			// homes, so ask the executable itself — skipping our own artifacts,
			// which the decision above already accounted for.
			active = foreignUpgradeStagingOver(resolvedPath, ownID)
		}
		if active != nil {
			if !override {
				return &blockedInPlaceInstallError{active: active, flag: flag}
			}
			log.WarningLog.Printf(
				"upgrade interlock: overridden — installing in place while daemon upgrade transaction %s (phase %s) is active; its rollback may no longer be usable",
				active.ID, active.Phase,
			)
		}
		// Refuses a symlinked destination (#3672). Both production callers hand
		// this an already-EvalSymlinks'd path, so a link here means the final
		// component became one between that resolution and this write — exactly
		// the state where swapping the binary is least safe. Refusing keeps the
		// in-place installer from writing an executable to a path nobody
		// resolved, and it is the same fail-closed polarity as the rest of this
		// interlock.
		return config.AtomicWriteFileRefusingLink(resolvedPath, binary, 0755)
	}

	// Check and write under the transaction preparation lock, so the two cannot
	// interleave. Checking and then writing is a time-of-check-to-time-of-use
	// window on its own: a transaction published in between is invisible to a
	// check that already passed, and the swap lands underneath it. upgradetxn's
	// Prepare takes this same lock and snapshots the running executable inside
	// it, so holding it here is what makes the interlock airtight rather than
	// merely likely.
	home, err := config.GetConfigDir()
	if err != nil {
		// No home to lock against. Proceeding matches the rest of this file's
		// polarity — an install must not be blocked by evidence we cannot read —
		// and an unresolvable home means there is no journal to race anyway.
		log.WarningLog.Printf("upgrade interlock: cannot resolve the agent-factory home to take the upgrade lock; installing unlocked: %v", err)
		return swap()
	}

	// A FAILURE TO TAKE THE LOCK MUST NEVER BLOCK THE INSTALL. Broken lock
	// storage — <home>/upgrade left as a file or symlink, or a directory that
	// cannot be read — says nothing about whether a transaction exists, and
	// returning that error here would be worse than the hazard this guard exists
	// to prevent: `af upgrade` permanently refusing, on every invocation, with
	// --ignore-active-upgrade powerless because the override lives inside the
	// swap that never runs. The journal check inside swap still applies, so the
	// unlocked path is today's behaviour plus a real guard, not a bypass.
	//
	// swapRan is what separates "the lock could not be taken" from "the swap
	// itself failed" — WithInstallLock returns fn's error too, so the error alone
	// cannot tell them apart, and mistaking a refusal for a lock failure would
	// install exactly what the guard just declined.
	swapRan := false
	var swapErr error
	take := upgradetxn.WithInstallLock
	if !mayWait {
		take = upgradetxn.TryWithInstallLock
	}
	lockErr := take(home, resolvedPath, func() error {
		swapRan = true
		swapErr = swap()
		return swapErr
	})
	if !swapRan && errors.Is(lockErr, upgradetxn.ErrInstallLockBusy) {
		// Another writer holds the locks and this caller must not wait. Nothing
		// was attempted, so the sentinel travels back for the caller to treat as
		// a deferral rather than a failed check — burning the six-hour window on
		// a swap that was never tried would suppress the next five hours of
		// legitimate ones.
		log.InfoLog.Printf("upgrade interlock: another upgrade holds the install lock; not waiting for it on this path")
		return lockErr
	}
	if swapRan {
		if lockErr != nil && swapErr == nil {
			log.WarningLog.Printf("upgrade interlock: installed, but releasing the upgrade lock failed: %v", lockErr)
		}
		return swapErr
	}
	log.WarningLog.Printf("upgrade interlock: cannot take the upgrade lock, so this install is not serialised against a daemon upgrade: %v", lockErr)
	return swap()
}

// upgradeOwningThisExecutable is the launch path's pre-throttle gate: a
// transaction in THIS home, or one staging beside the shared executable from any
// other home.
//
// The home journal alone is not enough here. Another AGENT_FACTORY_HOME can be
// mid-upgrade over the same binary, and that was previously discovered only
// inside the guarded writer — after the throttle cache was open and the archive
// downloaded, where the refusal recorded a failed check and suppressed
// launch-time updates for the next six hours over a transaction that takes
// seconds. Asking before the window opens costs one directory read and keeps
// both the bandwidth and the window.
//
// An executable that cannot be resolved is not treated as blocked: this gate
// only ever suppresses an update, and it must not do so on an inconclusive read.
func upgradeOwningThisExecutable() *activeUpgrade {
	active, ownID := activeUpgradeOwningExecutableWithID()
	if active != nil {
		return active
	}
	execPath, err := osExecutableFn()
	if err != nil {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return nil
	}
	return foreignUpgradeStagingOver(resolved, ownID)
}

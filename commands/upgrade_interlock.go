package commands

import (
	"errors"
	"fmt"
	"os"

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
// WHICH mechanism wins is still an open product decision on the issue (A: the
// transactional path is the single writer whenever a daemon owns the home;
// B: the daemon defers to the in-place installer; C: leave both and add mutual
// exclusion). This file does not pick one. It builds the floor ALL THREE need
// and that none of them can be safe without: the in-place writer never clobbers
// a live transaction. Landing it before activation means the reconciliation
// slice gets to be about policy rather than about safety.
//
// Nothing creates a transaction yet, so on every real box today this resolves to
// "no journal → proceed" and behaviour is unchanged. That is pinned by a test.

// ignoreActiveUpgradeFlag is the `af upgrade` escape hatch. Named for what it
// ignores rather than a bare --force, so the thing being overridden is visible
// in the command the user types.
const ignoreActiveUpgradeFlag = "ignore-active-upgrade"

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
//     box today.
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
	home, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("upgrade interlock: cannot resolve the agent-factory home; proceeding: %v", err)
		return nil
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	journal, err := loadUpgradeJournal(home)
	if errors.Is(err, upgradetxn.ErrNoActiveTransaction) {
		return nil
	}
	if err != nil {
		log.WarningLog.Printf("upgrade interlock: cannot read the daemon upgrade journal; proceeding with the in-place install: %v", err)
		return nil
	}
	if isTerminalUpgradePhase(journal.Phase) {
		return nil
	}
	return &activeUpgrade{
		ID:        journal.ID,
		ToVersion: journal.ToVersion,
		Phase:     string(journal.Phase),
	}
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

// writeExecutableInPlace is the ONE guarded in-place binary swap. Both installers
// go through it, so the interlock cannot be bypassed by adding a call site — the
// entrypoint checks elsewhere exist to give a better message and to skip a
// pointless download, not to be the guard.
//
// override is the caller's explicit "install anyway"; it is honoured, and logged,
// because an unoverridable auto-upgrade safeguard is its own hazard.
func writeExecutableInPlace(resolvedPath string, binary []byte, override bool, flag string) error {
	if active := activeUpgradeOwningExecutable(); active != nil {
		if !override {
			return &blockedInPlaceInstallError{active: active, flag: flag}
		}
		log.WarningLog.Printf(
			"upgrade interlock: overridden — installing in place while daemon upgrade transaction %s (phase %s) is active; its rollback may no longer be usable",
			active.ID, active.Phase,
		)
	}
	return config.AtomicWriteFile(resolvedPath, binary, 0755)
}

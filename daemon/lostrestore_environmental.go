package daemon

import (
	"context"
	"errors"
	"syscall"
	"time"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The environmental half of the restore retry policy (#5295).
//
// lostRestoreMaxAttempts is a budget against SESSION defects — the #3347
// give-up exists so a session that cannot come back (its agent exits, its
// worktree is gone, its state is inconsistent) stops retrying instead of
// flapping forever. None of that reasoning applies when the restore failed
// on the environment AROUND the session: a full disk, an exhausted fd table,
// a kernel OOM window, or a probe the environment never answered. Those
// conditions clear on their own — someone frees space, the OOM killer wins,
// the mount responds — without af doing anything, and abandoning the restore
// while they last strands a session that would have recovered on the very
// next attempt once the condition passed. That is the #5295 incident: an
// ENOSPC window failed six restores, the loop gave up, and the session sat
// claim_stale until a maintainer re-ran an archive by hand.
//
// So environmental failures get their own accounting: they are counted on
// their own streak, deferred by their own capped backoff, logged as the
// loop WAITING ON THE ENVIRONMENT, and never charged to the structural
// budget or written as the durable terminal LostRestoreFailure. A daemon
// restart loses only the streak count — the session is still Lost, still
// unresolved, and still wanted back, so the obligation survives restart by
// construction.
//
// Classification is by errors.Is through the wrapped chain — never by
// message text, which both misses wrapped errnos and false-matches prose
// ("no space left on device" in a string error must NOT classify).
func lostRestoreEnvironmentalErr(err error) bool {
	for _, errno := range lostRestoreEnvironmentalErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	// Two non-errno sentinels belong to the same class:
	//
	//   - context.DeadlineExceeded, produced by every bounded probe af runs
	//     (relocation identity checks, git command wrappers): the environment
	//     did not answer inside the bound — a waiting verdict by construction
	//     at every producer site, never evidence of a session defect.
	//   - sessiongit.ErrRelocateStateUnknown, af's own fail-closed "the state
	//     could not be established" verdict. Every producer designs for retry:
	//     an interrupted relocation's record is RESOLVED by the next claim,
	//     an "already in use" claim names an operation still in flight, and an
	//     unverifiable candidate keeps its evidence for the next attempt.
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, sessiongit.ErrRelocateStateUnknown)
}

// Resource-exhaustion errnos — the failure is a kernel-level resource limit,
// external to the session, which external relief resolves. Deliberately NOT
// included: EIO, EROFS, EACCES, EPERM (a permissions or media fault needs an
// operator, and waiting forever would hide it), EAGAIN ("would block" is a
// nonblocking-API artifact, not resource exhaustion), and any errno reached
// only through message text.
var lostRestoreEnvironmentalErrnos = []error{
	// A full filesystem is the #5295 incident itself: every write a restore
	// needs — gitdir repair, instance settlement, tmux's spawn — fails until
	// space is freed.
	syscall.ENOSPC,
	// Quota exhaustion is ENOSPC's twin: the same writes fail for accounting
	// rather than device reasons, and the same external relief resolves it.
	syscall.EDQUOT,
	// A kernel OOM window fails fork/exec and file-backed writes; it passes
	// when the memory pressure does, without af doing anything.
	syscall.ENOMEM,
	// A full fd table — the daemon's own (EMFILE) or the system-wide one
	// (ENFILE) — fails every open and spawn alike. The condition is daemon-
	// or machine-wide, not a property of this session, and it clears when
	// the descriptor pressure does.
	syscall.EMFILE,
	syscall.ENFILE,
	// A socket-level timeout is the network not answering — the same
	// verdict shape the remote-safety arms already treat as "keep holding,
	// the answer may come later".
	syscall.ETIMEDOUT,
}

// lostRestoreEnvBackoffMax caps the deferral between environmental retries.
// Environmental failures retry indefinitely — that is the point — but a disk
// that stays full must not spin hot: early attempts stay brisk so a short
// window heals fast, while a persistent condition settles at this ceiling.
var lostRestoreEnvBackoffMax = 30 * time.Minute

// lostRestoreEnvironmentalBackoff is the env-wait schedule: the same
// doubling shape as lostRestoreBackoff, capped higher. It shares the base
// var so the test seam (zeroRestoreBackoff) applies to both.
func lostRestoreEnvironmentalBackoff(attempt int) time.Duration {
	backoff := lostRestoreEnvBackoffMax
	// Same shift guard as lostRestoreBackoff: past ~16 doublings the
	// exponential form has no meaning and would overflow.
	if shift := attempt - 1; shift >= 0 && shift < 16 {
		if b := lostRestoreBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	return backoff
}

// armEnvironmentalRetry books the deferred retry for an environmental
// failure and returns the streak count and the delay chosen. Caller holds
// m.mu.
func (st *lostRestoreState) armEnvironmentalRetry() (int, time.Duration) {
	st.environmentalFailures++
	backoff := lostRestoreEnvironmentalBackoff(st.environmentalFailures)
	st.nextAttempt = time.Now().Add(backoff)
	return st.environmentalFailures, backoff
}

// restoreAttempts is the 1-based count of restore ATTEMPTS the episode has
// made: every failure of either class plus the spawn that just landed, so a
// session recovered through an environmental window still reports the tries
// it actually took ("restored after N attempts"). Caller holds m.mu.
func (st *lostRestoreState) restoreAttempts() int {
	return st.consecutiveFailures + st.environmentalFailures + 1
}

package daemon

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
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
// The wait is unbounded only for the errno class: a kernel resource limit is
// external to the session and so is its relief, so the loop waits as long as
// the condition lasts. The deadline class is bounded — a probe that never
// answers is indistinguishable from a permanently wedged relocation over the
// long haul — so it gets the same lostRestoreMaxAttempts share as the
// structural budget and then falls through to it (#5295 maintainer review).

// lostRestoreEnvClass is which environmental wait — if any — a restore
// failure earns.
type lostRestoreEnvClass int

const (
	// lostRestoreEnvNone is a structural failure: the bounded #3347 budget
	// owns it.
	lostRestoreEnvNone lostRestoreEnvClass = iota
	// lostRestoreEnvErrno is a kernel resource limit. The condition is
	// external and so is its relief, so the wait is unbounded — the #5295
	// property. Accepted worst case: a permanently full disk (or an fd
	// table or rlimit that never frees) waits forever instead of giving up.
	lostRestoreEnvErrno
	// lostRestoreEnvDeadline is a bounded probe that could not answer —
	// af's relocation probe pair (deadline + relocate-state sentinel), the
	// setup-side bounded git calls on the rebuild path (cleanup-stall and
	// removal-ownership sentinels), or the remote preserve push's transport
	// deadline at that call site. The same verdict shape a permanently
	// stuck condition produces, so the wait is bounded by
	// lostRestoreMaxAttempts and then charged to the structural budget.
	lostRestoreEnvDeadline
)

// Classification is by errors.Is through the wrapped chain — never by
// message text, which both misses wrapped errnos and false-matches prose
// ("no space left on device" in a string error must NOT classify).
func lostRestoreEnvironmentalClass(err error) lostRestoreEnvClass {
	for _, errno := range lostRestoreEnvironmentalErrnos {
		if errors.Is(err, errno) {
			return lostRestoreEnvErrno
		}
	}
	// A remote agent-server that verified a kernel resource limit inside the
	// sandbox says so with a machine-readable envelope code, reconstructed
	// client-side as this sentinel — the errno itself cannot cross the wire,
	// but its class can (#5295 review).
	if errors.Is(err, apiproto.ErrRemoteResourceExhausted) {
		return lostRestoreEnvErrno
	}
	// Neither half of an environmental non-answer signature is exempt on its
	// own. context.DeadlineExceeded alone is ambiguous — a permanently hung
	// user launch_cmd wraps it too, and that is a session defect, not an
	// outage. And each sentinel alone is a definitive verdict a retry cannot
	// change: ErrRelocateStateUnknown is "this path no longer identifies the
	// recorded worktree" — a session fact — while ErrWorktreeSetupStalled
	// without its deadline marks a stall detected some other way. Paired with
	// the deadline they are exactly what af's own bounded probes report when
	// the environment did not answer inside the bound: the deadline names the
	// non-answer and the sentinel names the bounded probe that could not
	// establish state — the relocation identity probe, the setup cleanup
	// step killed at its deadline by clearStaleWorktreePath on the rebuild
	// path, or the bounded `git worktree list` behind
	// requireSetupRemovalOwnership, which wraps its local-git deadline in
	// ErrSetupRemovalOwnershipUnproven before any mutation can be tried —
	// a repository-filesystem stall the next retry re-probes from scratch.
	// A bare ownership refusal (the sentinel without the deadline) stays a
	// definitive verdict and remains structural. Every other timeout and
	// every definitive verdict stays on the structural budget (#5295
	// review).
	if errors.Is(err, context.DeadlineExceeded) &&
		(errors.Is(err, sessiongit.ErrRelocateStateUnknown) ||
			errors.Is(err, sessiongit.ErrWorktreeSetupStalled) ||
			errors.Is(err, sessiongit.ErrSetupRemovalOwnershipUnproven)) {
		return lostRestoreEnvDeadline
	}
	return lostRestoreEnvNone
}

// lostRestoreResourceErrno reports whether the chain carries one of the
// kernel resource limits — exported within the package so the HTTP layer can
// emit the matching envelope code when an error crosses the wire.
func lostRestoreResourceErrno(err error) bool {
	for _, errno := range lostRestoreEnvironmentalErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// lostRestoreEnvironmentalErr is the bool form of
// lostRestoreEnvironmentalClass for call sites that only need the verdict.
func lostRestoreEnvironmentalErr(err error) bool {
	return lostRestoreEnvironmentalClass(err) != lostRestoreEnvNone
}

// Resource-exhaustion errnos — the failure is a kernel-level resource limit,
// external to the session, which external relief resolves. Deliberately NOT
// included: EIO, EROFS, EACCES, EPERM (a permissions or media fault needs an
// operator, and waiting forever would hide it), and any errno reached only
// through message text.
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
	// A fork/clone that hits the user's RLIMIT_NPROC or the cgroup
	// pids.max/TasksMax limit fails with EAGAIN — the same machine-wide
	// resource window as EMFILE, with the same external relief. The
	// producers are every exec.Cmd this path spawns: the tmux new-session
	// and send-keys calls behind a respawn, and the git worktree-repair
	// and preserve-push subprocesses; os.StartProcess surfaces the clone
	// errno wrapped, so errors.Is sees it. The nonblocking-I/O sense of
	// EAGAIN cannot reach this classifier: Go's poll runtime retries it
	// internally on blocking descriptors.
	syscall.EAGAIN,
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
// failure and returns the streak count and the delay chosen. absorbed is
// false when the failure belongs to the deadline class and that class has
// already spent its lostRestoreMaxAttempts share of the episode — the caller
// then charges the structural budget instead of arming another wait, so a
// probe that never answers still reaches the durable give-up. Caller holds
// m.mu.
func (st *lostRestoreState) armEnvironmentalRetry(class lostRestoreEnvClass) (attempts int, backoff time.Duration, absorbed bool) {
	if class == lostRestoreEnvDeadline &&
		st.environmentalDeadlineFailures >= lostRestoreMaxAttempts {
		return 0, 0, false
	}
	st.environmentalFailures++
	st.totalAttempts++
	if class == lostRestoreEnvDeadline {
		st.environmentalDeadlineFailures++
	}
	backoff = lostRestoreEnvironmentalBackoff(st.environmentalFailures)
	st.nextAttempt = time.Now().Add(backoff)
	return st.environmentalFailures, backoff, true
}

// restoreAttempts is the 1-based count of restore ATTEMPTS the episode has
// made: every failure recorded on the state plus the spawn that just
// landed. It reads the cumulative counter rather than the streaks — a
// structural failure resets the environmental streak, so summing streaks
// would under-report a mixed episode ("restored after 2 attempts" for a
// recovery that took seven) — while still counting the tries it actually
// took (#5295 review). Caller holds m.mu.
func (st *lostRestoreState) restoreAttempts() int {
	return st.totalAttempts + 1
}

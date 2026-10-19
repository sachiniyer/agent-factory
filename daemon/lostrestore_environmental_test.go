package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// TestLostRestoreEnvironmentalErr pins the #5295 classification: transient
// resource-exhaustion errnos are environmental AND wait unbounded, while the
// deadline + relocate-sentinel pair af's bounded probes produce for a
// non-answer is environmental but bounded. A session defect, a permissions
// fault, a bare deadline (a hung launch_cmd wraps it too — a session defect),
// a bare "state unknown" sentinel (a definitive verdict — the worktree is at
// no candidate), or a string that merely SAYS "no space left" is structural —
// classification runs through errors.Is on the wrapped chain, never through
// message text.
func TestLostRestoreEnvironmentalErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want lostRestoreEnvClass
	}{
		{"enospc bare", syscall.ENOSPC, lostRestoreEnvErrno},
		{"enospc wrapped", fmt.Errorf("recover: %w", syscall.ENOSPC), lostRestoreEnvErrno},
		{"enospc in PathError", &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, lostRestoreEnvErrno},
		{"enospc joined", errors.Join(errors.New("git failed"), fmt.Errorf("inner: %w", syscall.ENOSPC)), lostRestoreEnvErrno},
		// The worktree-rebuild shape: respawn wraps the missing-worktree ENOENT
		// AND the still-full-disk rebuild cause — the classifier must see the
		// second %w verb's errno, not just the first (#5295 review).
		{"enospc rebuild cause alongside enoent", &session.WorktreeUnavailableError{
			Title: "t", WorktreePath: "/x",
			Err: fmt.Errorf("%w (rebuild from existing branch failed: %w)", syscall.ENOENT, fmt.Errorf("mkdir: %w", syscall.ENOSPC)),
		}, lostRestoreEnvErrno},
		{"edquot", fmt.Errorf("persist: %w", syscall.EDQUOT), lostRestoreEnvErrno},
		{"enomem", fmt.Errorf("spawn: %w", syscall.ENOMEM), lostRestoreEnvErrno},
		{"emfile", fmt.Errorf("open: %w", syscall.EMFILE), lostRestoreEnvErrno},
		{"enfile", fmt.Errorf("open: %w", syscall.ENFILE), lostRestoreEnvErrno},
		{"etimedout", fmt.Errorf("probe: %w", syscall.ETIMEDOUT), lostRestoreEnvErrno},
		{"deadline carrying relocate sentinel", errors.Join(context.DeadlineExceeded, sessiongit.ErrRelocateStateUnknown), lostRestoreEnvDeadline},
		// clearStaleWorktreePath's real producer shape: the stalled sentinel and
		// the killed step's deadline wrapped in one fmt.Errorf chain (#5295 review).
		{"deadline carrying setup-stalled sentinel", fmt.Errorf(
			"clear stale worktree path: %w: killed at its deadline: %w",
			sessiongit.ErrWorktreeSetupStalled, context.DeadlineExceeded), lostRestoreEnvDeadline},
		{"setup-stalled sentinel bare", fmt.Errorf("cleanup: %w", sessiongit.ErrWorktreeSetupStalled), lostRestoreEnvNone},
		// requireSetupRemovalOwnership's real producer shape: the bounded
		// `git worktree list` inside it dies at the local-git deadline on a
		// stalled repository filesystem, and both the sentinel and the
		// deadline survive its %w wrap (#5295 review).
		{"deadline carrying ownership-unproven sentinel", fmt.Errorf(
			"%w: cannot verify the registered branch at /wt: git worktree list timed out: %w",
			sessiongit.ErrSetupRemovalOwnershipUnproven, context.DeadlineExceeded), lostRestoreEnvDeadline},
		// A bare ownership refusal — a detached or differently-branched
		// occupant — is a definitive verdict no retry changes: structural.
		{"ownership-unproven sentinel bare", fmt.Errorf(
			"%w: refusing to remove worktree /wt: git registers branch %q there",
			sessiongit.ErrSetupRemovalOwnershipUnproven, "refs/heads/other"), lostRestoreEnvNone},
		// The remote archive RPC's machine-readable class: the sandbox's agent
		// server verified a resource errno and the client reconstructed the
		// sentinel — the errno itself cannot cross the wire (#5295 review).
		{"remote resource-exhaustion sentinel", fmt.Errorf("%s: %w", "archive failed", apiproto.ErrRemoteResourceExhausted), lostRestoreEnvErrno},
		{"relocate unknown carrying enospc", errors.Join(fmt.Errorf("probe: %w", syscall.ENOSPC), sessiongit.ErrRelocateStateUnknown), lostRestoreEnvErrno},
		// The #5295 play-test producer: `git worktree add` fails with ENOSPC
		// trapped in *exec.ExitError stderr text — invisible to errors.Is. The
		// Go-side write probe supplies the errno; errors.Join carries both.
		{"subprocess exiterr joined with probe enospc", errors.Join(
			errors.New("git command failed: fatal: could not write (exit status 128)"),
			fmt.Errorf("write probe of /wt: %w", syscall.ENOSPC)), lostRestoreEnvErrno},
		{"subprocess exiterr joined with nonspace probe", errors.Join(
			errors.New("git command failed: exit status 128"),
			fmt.Errorf("write probe of /wt: %w", syscall.ENOENT)), lostRestoreEnvNone},

		// A bare deadline is ambiguous — a permanently hung launch_cmd wraps
		// it too, and that is a session defect, not an outage.
		{"deadline exceeded bare", fmt.Errorf("probe: %w", context.DeadlineExceeded), lostRestoreEnvNone},
		{"deadline exceeded launch_cmd style", fmt.Errorf("backend=hook: launch_cmd failed for %q: %w", "t", context.DeadlineExceeded), lostRestoreEnvNone},
		// The fsprobe capacity flight's own non-answer: a filesystem that
		// accepts the failed git subprocess but then blocks the diagnostic
		// write reports ErrProbeTimedOut + deadline — the same bounded
		// environmental class af's other probes produce. The sentinel alone
		// is a definitive verdict and stays structural (#5295 review).
		{"deadline carrying fsprobe sentinel", errors.Join(
			fsprobe.ErrProbeTimedOut, context.DeadlineExceeded), lostRestoreEnvDeadline},
		{"fsprobe sentinel bare", fmt.Errorf("space probe: %w", fsprobe.ErrProbeTimedOut), lostRestoreEnvNone},
		// Start's PRE-spawn has-session gate: a wedged shared tmux server
		// that cannot answer the probe reports ErrTmuxServerNoAnswer +
		// deadline — the same bounded environmental class af's other
		// non-answer sentinels produce. The sentinel alone, and every other
		// tmux timeout (the post-spawn readiness deadline can mean the pane
		// command exited instantly), stay structural (#5295 review).
		{"deadline carrying tmux server no-answer", errors.Join(
			sessiontmux.ErrTmuxServerNoAnswer, context.DeadlineExceeded), lostRestoreEnvDeadline},
		{"tmux server no-answer bare", fmt.Errorf("start: %w", sessiontmux.ErrTmuxServerNoAnswer), lostRestoreEnvNone},
		{"tmux timeout bare", fmt.Errorf("start: %w", sessiontmux.ErrTmuxTimeout), lostRestoreEnvNone},
		// The bare sentinel is a definitive verdict — "the recorded worktree
		// is at no candidate" — not a waiting one; retrying cannot change it.
		{"relocate state unknown bare", errors.Join(errors.New("cannot resolve"), sessiongit.ErrRelocateStateUnknown), lostRestoreEnvNone},
		{"relocate unknown carrying non-errno text", errors.Join(errors.New("identity mismatch"), sessiongit.ErrRelocateStateUnknown), lostRestoreEnvNone},

		{"nil", nil, lostRestoreEnvNone},
		{"plain structural error", errors.New("worktree unavailable"), lostRestoreEnvNone},
		{"string says no space but no errno", errors.New("no space left on device"), lostRestoreEnvNone},
		{"enoent", syscall.ENOENT, lostRestoreEnvNone},
		{"enoent wrapped", fmt.Errorf("stat: %w", syscall.ENOENT), lostRestoreEnvNone},
		{"eacces", syscall.EACCES, lostRestoreEnvNone},
		{"eperm", syscall.EPERM, lostRestoreEnvNone},
		{"eio", syscall.EIO, lostRestoreEnvNone},
		{"erofs", syscall.EROFS, lostRestoreEnvNone},
		{"eagain", syscall.EAGAIN, lostRestoreEnvErrno}, // fork/clone under RLIMIT_NPROC/pids.max via the tmux+git exec.Cmd producers; Go retries nonblocking EAGAIN internally
		{"context canceled", context.Canceled, lostRestoreEnvNone},
		{"flap sentinel", errRestoreFlapped, lostRestoreEnvNone},
		{"died before confirm sentinel", errRestoreDiedBeforeConfirm, lostRestoreEnvNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lostRestoreEnvironmentalClass(tc.err); got != tc.want {
				t.Fatalf("lostRestoreEnvironmentalClass(%v) = %v, want %v", tc.err, got, tc.want)
			}
			if got := lostRestoreEnvironmentalErr(tc.err); got != (tc.want != lostRestoreEnvNone) {
				t.Fatalf("lostRestoreEnvironmentalErr(%v) = %v, want %v", tc.err, got, tc.want != lostRestoreEnvNone)
			}
		})
	}
}

// TestLostRestoreEnvironmentalBackoff pins the wait schedule: the same
// doubling as the structural backoff, capped at its own ceiling so a disk
// that stays full defers instead of spinning hot.
func TestLostRestoreEnvironmentalBackoff(t *testing.T) {
	prevBase, prevMax := lostRestoreBackoffBase, lostRestoreEnvBackoffMax
	lostRestoreBackoffBase, lostRestoreEnvBackoffMax = time.Second, time.Minute
	t.Cleanup(func() { lostRestoreBackoffBase, lostRestoreEnvBackoffMax = prevBase, prevMax })

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	}
	for attempt := 1; attempt <= len(want); attempt++ {
		if got := lostRestoreEnvironmentalBackoff(attempt); got != want[attempt-1] {
			t.Fatalf("attempt %d: backoff = %s, want %s", attempt, got, want[attempt-1])
		}
	}
	if got := lostRestoreEnvironmentalBackoff(100); got != time.Minute {
		t.Fatalf("attempt 100: backoff = %s, want the %s cap (no overflow)", got, time.Minute)
	}
}

// TestRestoreLostSessions_EnvironmentalFailuresNeverGiveUp is the #5295
// contract end to end: every failure environmental (wrapped ENOSPC, the
// incident's actual class) means the loop keeps trying past the structural
// budget — no durable LostRestoreFailure, no give-up line, and the state
// keeps the session restorable for the pass where the disk frees.
func TestRestoreLostSessions_EnvironmentalFailuresNeverGiveUp(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    fmt.Errorf("recover: failed to write worktree state: %w", syscall.ENOSPC),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "disk-full", backend, true, session.Lost)

	// Far past the structural budget: every pass must still attempt, because
	// none of the failures is the session's fault.
	passes := 2*lostRestoreMaxAttempts + 2
	for i := 0; i < passes; i++ {
		manager.RestoreLostSessions()
	}

	if got := backend.recoverCalls(); got != passes {
		t.Fatalf("recover calls = %d, want %d — environmental failures must keep retrying past the %d-attempt structural budget", got, passes, lostRestoreMaxAttempts)
	}
	if inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = true: a purely environmental episode must never reach the durable give-up")
	}
	if data := inst.ToInstanceData(); data.LostRestoreFailure != nil {
		t.Fatalf("durable terminal failure written for an environmental episode: %#v", data.LostRestoreFailure)
	}
	// The wait is visible on the session the same way the gave-up verdict
	// is: the row reports what it is waiting on, with the error that armed
	// it, for as long as the condition lasts (#5295 maintainer review).
	if !inst.LifecycleView().RestoreWaitingOnEnvironment {
		t.Fatal("RestoreWaitingOnEnvironment = false: an armed environmental retry must be visible on the session")
	}
	if wait := inst.LostRestoreWaitSnapshot(); wait == nil || wait.Attempts != passes {
		t.Fatalf("LostRestoreWaitSnapshot = %#v, want the armed wait with %d attempts", wait, passes)
	}
	if reason, _, _, _ := inst.IdleReasonDetailSnapshot(); reason != session.IdleReasonRestoreWaitingOnEnvironment {
		t.Fatalf("idle reason = %q, want %q while the environment-wait is armed", reason, session.IdleReasonRestoreWaitingOnEnvironment)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	var consecutive, environmental, total int
	if st != nil {
		consecutive, environmental, total = st.consecutiveFailures, st.environmentalFailures, st.totalAttempts
	}
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state vanished: the waiting episode must stay tracked for the next pass")
	}
	if consecutive != 0 {
		t.Fatalf("consecutiveFailures = %d, want 0 — environmental failures must not charge the structural budget", consecutive)
	}
	if environmental != passes {
		t.Fatalf("environmentalFailures = %d, want %d", environmental, passes)
	}
	if total != passes {
		t.Fatalf("totalAttempts = %d, want %d — the cumulative counter must count every failure class", total, passes)
	}
	if !strings.Contains(ownLogs.warnings.String(), "waiting on the environment") {
		t.Fatalf("the wait must say what it is waiting on; warn logs:\n%s", ownLogs.warnings.String())
	}
	if strings.Contains(ownLogs.errors.String(), "giving up") {
		t.Fatalf("a give-up line was emitted for a purely environmental episode; error logs:\n%s", ownLogs.errors.String())
	}
	// Nothing terminal was persisted either: a restart reloads the session as
	// an ordinary wanted Lost row and the obligation continues.
	if rec := recordFor(t, repoID, "disk-full"); rec != nil && rec.LostRestoreFailure != nil {
		t.Fatalf("persisted LostRestoreFailure for an environmental episode: %#v", rec.LostRestoreFailure)
	}
}

// TestRestoreLostSessions_EnvironmentalClearsToRecover is the recovery half
// of #5295: once the condition clears, the next pass restores the session
// with no manual action — no operator re-archive needed. A structural
// failure interleaved in the window must neither derail the recovery nor
// shrink the reported attempt count: the log counts every try the episode
// actually made.
func TestRestoreLostSessions_EnvironmentalClearsToRecover(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	environmental := fmt.Errorf("recover: %w", syscall.ENOSPC)
	structural := errors.New("recover: agent exited at startup")
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    environmental,
	}
	inst := registerStarted(t, manager, repoID, repoPath, "disk-frees", backend, true, session.Lost)

	for i := 0; i < lostRestoreMaxAttempts-1; i++ {
		manager.RestoreLostSessions()
	}
	backend.mu.Lock()
	backend.failWith = structural
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running once the environmental condition cleared", got)
	}
	// A spawn that returned proves the environment answered: the wait the
	// ENOSPC armed is stale the moment the attempt goes through (#5295).
	if inst.LifecycleView().RestoreWaitingOnEnvironment {
		t.Fatal("RestoreWaitingOnEnvironment still set after a spawn returned — the wait must clear on success")
	}
	confirmAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("a confirmed-alive recovery must drop the whole episode state, environmental streak included")
	}
	// 5 environmental + 1 structural failure + the spawn that landed = 7
	// attempts; summing the streaks would report 2.
	want := fmt.Sprintf("restored after %d attempts", lostRestoreMaxAttempts+1)
	if !strings.Contains(ownLogs.info.String(), want) {
		t.Fatalf("attempt log must count every failure class cumulatively; want %q in info logs:\n%s", want, ownLogs.info.String())
	}
}

// TestRestoreLostSessions_StructuralStillGivesUpAmidEnvironmental pins the
// #3347 half of the contract: structural failures keep their bounded budget
// even when environmental failures are interleaved — an ENOSPC window
// neither counts toward nor resets the structural streak.
func TestRestoreLostSessions_StructuralStillGivesUpAmidEnvironmental(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	structural := errors.New("recover: agent exited at startup")
	environmental := fmt.Errorf("recover: %w", syscall.ENOSPC)
	backend := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
	inst := registerStarted(t, manager, repoID, repoPath, "mixed", backend, true, session.Lost)

	// Alternate environmental/structural failures. The sixth structural
	// failure must give up exactly as if the environmental ones were absent.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		backend.mu.Lock()
		backend.failWith = environmental
		backend.mu.Unlock()
		manager.RestoreLostSessions()

		backend.mu.Lock()
		backend.failWith = structural
		backend.mu.Unlock()
		manager.RestoreLostSessions()
	}

	if got := backend.recoverCalls(); got != 2*lostRestoreMaxAttempts {
		t.Fatalf("recover calls = %d, want %d (each failure class attempted once per pass)", got, 2*lostRestoreMaxAttempts)
	}
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("LostRestoreGaveUp = false after six interleaved structural failures: the #3347 budget must still terminate")
	}
	data := inst.ToInstanceData()
	if data.LostRestoreFailure == nil || data.LostRestoreFailure.Error != structural.Error() {
		t.Fatalf("surfaced failure = %#v, want the structural reason, not the environmental one", data.LostRestoreFailure)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	envStreak, total := 0, 0
	if st != nil {
		envStreak, total = st.environmentalFailures, st.totalAttempts
	}
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state vanished after give-up: the terminal episode must stay recorded")
	}
	if envStreak != 0 {
		t.Fatalf("environmentalFailures = %d after a structural failure, want the streak ended", envStreak)
	}
	if total != 2*lostRestoreMaxAttempts {
		t.Fatalf("totalAttempts = %d, want %d — the cumulative counter must count both classes", total, 2*lostRestoreMaxAttempts)
	}

	// And the terminal session stays stopped even though the environment is
	// now healthy — give-up is a durable verdict, not a schedule.
	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != 2*lostRestoreMaxAttempts {
		t.Fatalf("recover calls after give-up = %d, want still %d", got, 2*lostRestoreMaxAttempts)
	}
}

// TestRestoreLostSessions_EnvironmentalBackoffDefers: environmental retries
// honor their backoff like structural ones — the fix is a longer leash, not
// a hot loop against a disk that stays full.
func TestRestoreLostSessions_EnvironmentalBackoffDefers(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	prev := lostRestoreBackoffBase
	lostRestoreBackoffBase = time.Hour
	t.Cleanup(func() { lostRestoreBackoffBase = prev })
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    fmt.Errorf("recover: %w", syscall.ENOSPC),
	}
	registerStarted(t, manager, repoID, repoPath, "waiting", backend, true, session.Lost)

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	if got := backend.recoverCalls(); got != 1 {
		t.Fatalf("passes inside the environmental backoff window must not re-attempt: want 1, got %d", got)
	}
}

// TestRestoreLostSessions_DeadlineClassFallsThroughAfterBoundedShare pins the
// bound the maintainer required on the deadline class (#5295 maintainer
// review): a probe that never answers is indistinguishable from a permanently
// wedged relocation over the long haul, so after lostRestoreMaxAttempts
// environmental waits the next deadline-class failure charges the structural
// budget — only the errno class waits unbounded.
func TestRestoreLostSessions_DeadlineClassFallsThroughAfterBoundedShare(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	probeNoAnswer := errors.Join(context.DeadlineExceeded, sessiongit.ErrRelocateStateUnknown)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    probeNoAnswer,
	}
	inst := registerStarted(t, manager, repoID, repoPath, "wedged-probe", backend, true, session.Lost)

	// The class's full bounded share is absorbed environmentally first.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	var consecutive, deadline int
	if st != nil {
		consecutive, deadline = st.consecutiveFailures, st.environmentalDeadlineFailures
	}
	manager.mu.Unlock()
	if consecutive != 0 || deadline != lostRestoreMaxAttempts {
		t.Fatalf("after %d absorbed waits: consecutiveFailures = %d, environmentalDeadlineFailures = %d — the bounded share must be tracked separately", lostRestoreMaxAttempts, consecutive, deadline)
	}

	// The next non-answer charges the structural budget: the environmental
	// streak ends, the structural one starts, and the session stops claiming
	// it is waiting on the environment. The spent deadline share stays spent
	// for the rest of the episode — it is not re-armed by the charge, or
	// every charge would grant the class another six free waits and a wedged
	// probe would alternate structural/environmental without ever escalating
	// (#5295 review).
	manager.RestoreLostSessions()
	manager.mu.Lock()
	st = manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	if st != nil {
		consecutive, deadline = st.consecutiveFailures, st.environmentalDeadlineFailures
	}
	manager.mu.Unlock()
	if consecutive != 1 {
		t.Fatalf("consecutiveFailures = %d after the bounded share was spent, want 1 — the non-answer must charge the structural budget", consecutive)
	}
	if deadline != lostRestoreMaxAttempts {
		t.Fatalf("environmentalDeadlineFailures = %d after the structural charge, want %d — the share is episode-scoped and stays spent", deadline, lostRestoreMaxAttempts)
	}
	if inst.LifecycleView().RestoreWaitingOnEnvironment {
		t.Fatal("RestoreWaitingOnEnvironment still set after the failure went structural — the wait must clear")
	}
	if !strings.Contains(ownLogs.warnings.String(), "charges the structural budget") {
		t.Fatalf("the deadline-class exhaustion must be visible in the warn log:\n%s", ownLogs.warnings.String())
	}

	// And the structural budget still terminates: with the deadline share
	// spent for the episode, each further non-answer charges structural
	// directly — five more reach the durable give-up instead of waiting
	// forever.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("a deadline-class probe that never answers must still reach the durable give-up")
	}
}

// TestRestoreLostSessions_StructuralFailureClearsEnvironmentWait: the wait is
// a claim about why the loop is deferring — the moment a failure is the
// session's own, the row stops claiming the environment (#5295 maintainer
// review).
func TestRestoreLostSessions_StructuralFailureClearsEnvironmentWait(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    fmt.Errorf("recover: %w", syscall.ENOSPC),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "turns-structural", backend, true, session.Lost)

	manager.RestoreLostSessions()
	if !inst.LifecycleView().RestoreWaitingOnEnvironment {
		t.Fatal("setup: the ENOSPC failure must have armed the environmental wait")
	}

	backend.mu.Lock()
	backend.failWith = errors.New("recover: agent exited at startup")
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if inst.LifecycleView().RestoreWaitingOnEnvironment {
		t.Fatal("RestoreWaitingOnEnvironment still set after a structural failure")
	}
	if inst.LostRestoreWaitSnapshot() != nil {
		t.Fatalf("LostRestoreWaitSnapshot = %#v after a structural failure, want cleared", inst.LostRestoreWaitSnapshot())
	}
	// And the retirement is durable: the ENOSPC pass persisted the wait, so
	// only a fresh write can retire it — a restart on the stale row would
	// resurrect "waiting on the environment" for a failure that was the
	// session's own (#5295 review).
	if rec := recordFor(t, repoID, "turns-structural"); rec != nil && rec.LostRestoreWait != nil {
		t.Fatalf("persisted LostRestoreWait = %#v after a structural failure, want the stale wait retired on disk", rec.LostRestoreWait)
	}
}

// TestRestoreLostSessions_EnvironmentalFailureAfterGiveUpPromisesNothing is
// the honesty half of the durable gate: a manual restore on a session that
// already carries its terminal verdict cannot arm an automatic retry —
// lostSessionWantsRestore refuses it — so the log must name the real
// off-ramp (another explicit restore) instead of promising "retrying in …"
// (#5295 review).
func TestRestoreLostSessions_EnvironmentalFailureAfterGiveUpPromisesNothing(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    errors.New("recover: agent exited at startup"),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "terminal", backend, true, session.Lost)

	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.RestoreLostSessions()
	}
	if !inst.LifecycleView().LostRestoreGaveUp {
		t.Fatal("setup: six structural failures must reach the durable give-up")
	}

	// Drop the setup's own warnings (the give-up arm logs "retrying in …"
	// for attempts 1-5) so the checks below see only this failure's report.
	ownLogs.warnings.Reset()
	manager.recordLostRestoreFailure(daemonInstanceKey(repoID, "terminal"), repoID, inst,
		fmt.Errorf("recover: %w", syscall.ENOSPC), lostRestoreManual)

	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state vanished: the recorded failure must stay tracked")
	}
	if st.environmentalFailures != 0 {
		t.Fatalf("environmentalFailures = %d: a gave-up session must not accumulate an environmental streak", st.environmentalFailures)
	}
	if !st.nextAttempt.IsZero() {
		t.Fatal("an environmental retry was armed for a session the entry gate refuses — the promised retry can never run")
	}
	warnings := ownLogs.warnings.String()
	if strings.Contains(warnings, "waiting on the environment") {
		t.Fatalf("the env arm promised a retry the durable gate refuses; warn logs:\n%s", warnings)
	}
	if !strings.Contains(warnings, "explicit restore") {
		t.Fatalf("the honest report names the real off-ramp; warn logs:\n%s", warnings)
	}

	// And the loop itself still refuses the session — nothing re-armed it.
	manager.RestoreLostSessions()
	if got := backend.recoverCalls(); got != lostRestoreMaxAttempts {
		t.Fatalf("recover calls = %d, want still %d — gave-up stays terminal", got, lostRestoreMaxAttempts)
	}
}

// TestRestoreLostSessions_EnvironmentalWaitPersistFailureStaysOffSettleOwed
// pins the write class of the wait (#5295 review): the wait is a re-derivable
// advisory fact — the next arm re-arms and re-writes it at the restore's own
// backoff — while the generic settleOwed retry runs on every poll tick. A
// failed wait write must warn and drop, never join the per-poll retry, or a
// full disk gets hammered by settlement retries during the very incident the
// arm exists to tolerate.
func TestRestoreLostSessions_EnvironmentalWaitPersistFailureStaysOffSettleOwed(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    fmt.Errorf("recover: %w", syscall.ENOSPC),
	}
	registerStarted(t, manager, repoID, repoPath, "unpersisted-wait", backend, true, session.Lost)

	// Every write for this row fails — the disk-full shape the wait exists
	// for — so each arm's persist is dropped.
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	persistAttempts := 0
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "unpersisted-wait" {
			persistAttempts++
			return fmt.Errorf("forced persist failure: %w", syscall.ENOSPC)
		}
		return nil
	}

	manager.RestoreLostSessions()
	manager.RestoreLostSessions()
	manager.RestoreLostSessions()

	manager.mu.Lock()
	owed := len(manager.settleOwed)
	manager.mu.Unlock()
	if owed != 0 {
		t.Fatalf("settleOwed = %d, want 0 — a dropped environmental-wait write must not join the every-poll settlement retry", owed)
	}
	if !strings.Contains(ownLogs.warnings.String(), "not yet durable") {
		t.Fatalf("a dropped wait write must still surface in the warn log:\n%s", ownLogs.warnings.String())
	}

	// The flush has nothing to retry for this row: no hidden poll-cadence
	// re-writes beyond what the arms themselves attempted.
	before := persistAttempts
	manager.FlushOwedSettlements()
	manager.FlushOwedSettlements()
	if persistAttempts != before {
		t.Fatalf("FlushOwedSettlements retried %d writes for the row — the dropped wait persist must not be owed", persistAttempts-before)
	}
}

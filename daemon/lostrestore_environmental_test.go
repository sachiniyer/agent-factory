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

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// TestLostRestoreEnvironmentalErr pins the #5295 classification: transient
// resource-exhaustion errnos and deadline verdicts are environmental; a
// session defect, a permissions fault, a bare "state unknown" sentinel (a
// definitive verdict — the worktree is at no candidate), or a string that
// merely SAYS "no space left" is not — classification runs through
// errors.Is on the wrapped chain, never through message text.
func TestLostRestoreEnvironmentalErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"enospc bare", syscall.ENOSPC, true},
		{"enospc wrapped", fmt.Errorf("recover: %w", syscall.ENOSPC), true},
		{"enospc in PathError", &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, true},
		{"enospc joined", errors.Join(errors.New("git failed"), fmt.Errorf("inner: %w", syscall.ENOSPC)), true},
		{"edquot", fmt.Errorf("persist: %w", syscall.EDQUOT), true},
		{"enomem", fmt.Errorf("spawn: %w", syscall.ENOMEM), true},
		{"emfile", fmt.Errorf("open: %w", syscall.EMFILE), true},
		{"enfile", fmt.Errorf("open: %w", syscall.ENFILE), true},
		{"etimedout", fmt.Errorf("probe: %w", syscall.ETIMEDOUT), true},
		{"deadline exceeded", fmt.Errorf("probe: %w", context.DeadlineExceeded), true},
		{"relocate unknown carrying deadline", errors.Join(context.DeadlineExceeded, sessiongit.ErrRelocateStateUnknown), true},
		{"relocate unknown carrying enospc", errors.Join(fmt.Errorf("probe: %w", syscall.ENOSPC), sessiongit.ErrRelocateStateUnknown), true},

		// The bare sentinel is a definitive verdict — "the recorded worktree
		// is at no candidate" — not a waiting one; retrying cannot change it.
		{"relocate state unknown bare", errors.Join(errors.New("cannot resolve"), sessiongit.ErrRelocateStateUnknown), false},
		{"relocate unknown carrying non-errno text", errors.Join(errors.New("identity mismatch"), sessiongit.ErrRelocateStateUnknown), false},

		{"nil", nil, false},
		{"plain structural error", errors.New("worktree unavailable"), false},
		{"string says no space but no errno", errors.New("no space left on device"), false},
		{"enoent", syscall.ENOENT, false},
		{"enoent wrapped", fmt.Errorf("stat: %w", syscall.ENOENT), false},
		{"eacces", syscall.EACCES, false},
		{"eperm", syscall.EPERM, false},
		{"eio", syscall.EIO, false},
		{"erofs", syscall.EROFS, false},
		{"eagain", syscall.EAGAIN, false},
		{"context canceled", context.Canceled, false},
		{"flap sentinel", errRestoreFlapped, false},
		{"died before confirm sentinel", errRestoreDiedBeforeConfirm, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lostRestoreEnvironmentalErr(tc.err); got != tc.want {
				t.Fatalf("lostRestoreEnvironmentalErr(%v) = %v, want %v", tc.err, got, tc.want)
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
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	var consecutive, environmental int
	if st != nil {
		consecutive, environmental = st.consecutiveFailures, st.environmentalFailures
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
// with no manual action — no operator re-archive needed.
func TestRestoreLostSessions_EnvironmentalClearsToRecover(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
		failWith:    fmt.Errorf("recover: %w", syscall.ENOSPC),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "disk-frees", backend, true, session.Lost)

	for i := 0; i < lostRestoreMaxAttempts+3; i++ {
		manager.RestoreLostSessions()
	}

	backend.mu.Lock()
	backend.failWith = nil
	backend.mu.Unlock()
	manager.RestoreLostSessions()

	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running once the environmental condition cleared", got)
	}
	confirmAlive(manager, repoID, inst)
	manager.RestoreLostSessions()
	manager.mu.Lock()
	_, hasState := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	manager.mu.Unlock()
	if hasState {
		t.Fatal("a confirmed-alive recovery must drop the whole episode state, environmental streak included")
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
	envStreak := 0
	if st != nil {
		envStreak = st.environmentalFailures
	}
	manager.mu.Unlock()
	if st == nil {
		t.Fatal("retry state vanished after give-up: the terminal episode must stay recorded")
	}
	if envStreak != 0 {
		t.Fatalf("environmentalFailures = %d after a structural failure, want the streak ended", envStreak)
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

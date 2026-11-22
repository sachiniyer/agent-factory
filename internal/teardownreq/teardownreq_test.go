package teardownreq

import (
	"errors"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

func proc(pid int, start uint64) proctree.Process {
	return proctree.Process{PID: pid, StartID: start}
}

var (
	targetA = TmuxTarget("af_a")
	targetB = TmuxTarget("af_b")
	targetW = WorktreeTarget("/repo/wt-a")
)

// One requester identity can be held by two overlapping handlers — net/rpc
// serves a connection's calls concurrently, and a second connection carries
// the same kernel-verified peer. The first unregister must not drop the
// identity the second handler still needs (#5182, Codex on #5186).
func TestTrackCountsOverlappingRegistrations(t *testing.T) {
	p := proc(4321, 99)
	un1 := Track(p, targetA)
	un2 := Track(p, targetA)
	if !Is(p, targetA) {
		t.Fatal("tracked identity must report Is")
	}
	un1()
	if !Is(p, targetA) {
		t.Fatal("first unregister dropped an identity a second registration still holds")
	}
	un2()
	if Is(p, targetA) {
		t.Fatal("last unregister must remove the identity")
	}
}

// An unregister is idempotent — a transport that both pops on reply write and
// drains on close must not underflow the count and unregister a live
// registration belonging to a different handler.
func TestUnregisterIsIdempotent(t *testing.T) {
	p := proc(4322, 7)
	keep := Track(p, targetA)
	un := Track(p, targetA)
	un()
	un()
	if !Is(p, targetA) {
		t.Fatal("double-unregister must not drop a registration it does not own")
	}
	keep()
	if Is(p, targetA) {
		t.Fatal("removing the last registration must clear the identity")
	}
}

// A recycled pid is a different process instance: the (pid, start-stamp) pair
// is the identity, never the pid alone — the issue's property (d).
func TestRecycledPIDIsDistinctIdentity(t *testing.T) {
	old := proc(4323, 1)
	recycled := proc(4323, 2)
	un := Track(old, targetA)
	defer un()
	if Is(recycled, targetA) {
		t.Fatal("a recycled pid must not inherit the prior owner's registration")
	}
}

// The exemption is scoped to the teardown the request is waiting on (#7754):
// a client blocked on archiving session B while it lives in session A's pane
// is a caller of B's teardown only — A's reaper must NOT excuse it, or a
// tightened scope re-creates the #7752 refusal in the other direction.
func TestTrackScopesExcusalToItsOwnTeardown(t *testing.T) {
	p := proc(4326, 11)
	un := Track(p, targetB)
	defer un()
	if !Is(p, targetB) {
		t.Fatal("a requester must be excused from the teardown its request owns")
	}
	if Is(p, targetA) {
		t.Fatal("a requester must NOT be excused from a teardown it did not ask for")
	}
	if Is(p, targetW) {
		t.Fatal("a tmux-named target must not match a worktree path of the same spelling")
	}
}

// Overlapping registrations for DIFFERENT targets stay independent: the
// handler for teardown A unregistering must not drop the exemption the
// still-blocked handler for teardown B holds.
func TestTrackScopesOverlappingRegistrationsByTarget(t *testing.T) {
	p := proc(4327, 13)
	unA := Track(p, targetA)
	defer unA()
	unB := Track(p, targetB)
	defer unB()
	unA()
	if !Is(p, targetB) {
		t.Fatal("unregistering teardown A's registration must not drop teardown B's")
	}
	if Is(p, targetA) {
		t.Fatal("teardown A's exemption ended with its own handler")
	}
}

// One registration covers every target its teardown owns — a session's tmux
// sessions AND its worktree — so the requester is excused from each of that
// teardown's reapers.
func TestTrackCoversAllOfItsTeardownsTargets(t *testing.T) {
	p := proc(4328, 17)
	un := Track(p, targetA, targetW)
	defer un()
	if !Is(p, targetA) || !Is(p, targetW) {
		t.Fatal("a session teardown's registration must spare its caller in the pane reap AND the writer reap")
	}
}

// SignalUnlessTracked is the atomic check-and-signal a reaper must use at
// signal time (#5182, Codex on #5186): a tracked requester is never
// signalled, and the decision shares the registry lock with Track so a
// registration landing mid-tier cannot be missed.
func TestSignalUnlessTrackedSkipsTracked(t *testing.T) {
	p := proc(4324, 5)
	un := Track(p, targetA)
	defer un()
	attempted, err := SignalUnlessTracked(p, targetA, syscall.SIGTERM)
	if attempted {
		t.Fatal("a tracked requester must not be signalled")
	}
	if err != nil {
		t.Fatalf("the exempt path must not error, got %v", err)
	}
	if !Is(p, targetA) {
		t.Fatal("a skipped signal must not consume the registration")
	}
}

// The scope is enforced at signal time too: a process tracked for another
// teardown is signalled like an untracked one (#7754).
func TestSignalUnlessTrackedSignalsForOtherTeardowns(t *testing.T) {
	p := proc(99999997, 1)
	un := Track(p, targetB)
	defer un()
	attempted, err := SignalUnlessTracked(p, targetA, syscall.SIGTERM)
	if !attempted {
		t.Fatal("a requester of another teardown must be signalled in this one")
	}
	if !errors.Is(err, proctree.ErrIdentityChanged) {
		t.Fatalf("a nonexistent pid should yield ErrIdentityChanged, got %v", err)
	}
}

// The same lock that exempts must signal: an untracked process is attempted
// (a dead pid fails identity validation downstream, not the gate itself).
func TestSignalUnlessTrackedSignalsUntracked(t *testing.T) {
	p := proc(99999999, 1)
	attempted, err := SignalUnlessTracked(p, targetA, syscall.SIGTERM)
	if !attempted {
		t.Fatal("an untracked process must be signalled")
	}
	if !errors.Is(err, proctree.ErrIdentityChanged) {
		t.Fatalf("a nonexistent pid should yield ErrIdentityChanged, got %v", err)
	}
}

// The spare predicate is the worktree reaper's last word before its kill
// (#7244, #7245 review R2). It runs inside the registry's critical section, so
// no lock wait separates the caller's read from the signal. A true answer
// spares the process, a false one signals it, and a tracked requester is
// spared without asking.
func TestSignalUnlessTrackedOrSparedEvaluatesSpareInsideTheLock(t *testing.T) {
	p := proc(99999998, 1)
	heldDuringSpare := false
	attempted, err := SignalUnlessTrackedOrSpared(p, targetW, syscall.SIGTERM, func(got proctree.Process) bool {
		if got != p {
			t.Errorf("spare was asked about %+v, want %+v", got, p)
		}
		if trackedMu.TryLock() {
			trackedMu.Unlock()
		} else {
			heldDuringSpare = true
		}
		return true
	})
	if !heldDuringSpare {
		t.Fatal("spare must run while the registry lock is held, with no wait between it and the signal")
	}
	if attempted || err != nil {
		t.Fatalf("a spared process must not be signalled: attempted=%t err=%v", attempted, err)
	}

	attempted, err = SignalUnlessTrackedOrSpared(p, targetW, syscall.SIGTERM, func(proctree.Process) bool { return false })
	if !attempted {
		t.Fatal("a process spare declines must be signalled")
	}
	if !errors.Is(err, proctree.ErrIdentityChanged) {
		t.Fatalf("a nonexistent pid should yield ErrIdentityChanged, got %v", err)
	}

	tracked := proc(4325, 6)
	un := Track(tracked, targetW)
	defer un()
	asked := false
	attempted, err = SignalUnlessTrackedOrSpared(tracked, targetW, syscall.SIGTERM, func(proctree.Process) bool { asked = true; return false })
	if attempted || err != nil || asked {
		t.Fatalf("a tracked requester is spared without consulting spare: attempted=%t err=%v asked=%t", attempted, err, asked)
	}
}

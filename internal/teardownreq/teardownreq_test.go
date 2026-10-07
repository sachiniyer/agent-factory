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

// One requester identity can be held by two overlapping handlers — net/rpc
// serves a connection's calls concurrently, and a second connection carries
// the same kernel-verified peer. The first unregister must not drop the
// identity the second handler still needs (#5182, Codex on #5186).
func TestTrackCountsOverlappingRegistrations(t *testing.T) {
	p := proc(4321, 99)
	un1 := Track(p)
	un2 := Track(p)
	if !Is(p) {
		t.Fatal("tracked identity must report Is")
	}
	un1()
	if !Is(p) {
		t.Fatal("first unregister dropped an identity a second registration still holds")
	}
	un2()
	if Is(p) {
		t.Fatal("last unregister must remove the identity")
	}
}

// An unregister is idempotent — a transport that both pops on reply write and
// drains on close must not underflow the count and unregister a live
// registration belonging to a different handler.
func TestUnregisterIsIdempotent(t *testing.T) {
	p := proc(4322, 7)
	keep := Track(p)
	un := Track(p)
	un()
	un()
	if !Is(p) {
		t.Fatal("double-unregister must not drop a registration it does not own")
	}
	keep()
	if Is(p) {
		t.Fatal("removing the last registration must clear the identity")
	}
}

// A recycled pid is a different process instance: the (pid, start-stamp) pair
// is the identity, never the pid alone — the issue's property (d).
func TestRecycledPIDIsDistinctIdentity(t *testing.T) {
	old := proc(4323, 1)
	recycled := proc(4323, 2)
	un := Track(old)
	defer un()
	if Is(recycled) {
		t.Fatal("a recycled pid must not inherit the prior owner's registration")
	}
}

// SignalUnlessTracked is the atomic check-and-signal a reaper must use at
// signal time (#5182, Codex on #5186): a tracked requester is never
// signalled, and the decision shares the registry lock with Track so a
// registration landing mid-tier cannot be missed.
func TestSignalUnlessTrackedSkipsTracked(t *testing.T) {
	p := proc(4324, 5)
	un := Track(p)
	defer un()
	attempted, err := SignalUnlessTracked(p, syscall.SIGTERM)
	if attempted {
		t.Fatal("a tracked requester must not be signalled")
	}
	if err != nil {
		t.Fatalf("the exempt path must not error, got %v", err)
	}
	if !Is(p) {
		t.Fatal("a skipped signal must not consume the registration")
	}
}

// The same lock that exempts must signal: an untracked process is attempted
// (a dead pid fails identity validation downstream, not the gate itself).
func TestSignalUnlessTrackedSignalsUntracked(t *testing.T) {
	p := proc(99999999, 1)
	attempted, err := SignalUnlessTracked(p, syscall.SIGTERM)
	if !attempted {
		t.Fatal("an untracked process must be signalled")
	}
	if !errors.Is(err, proctree.ErrIdentityChanged) {
		t.Fatalf("a nonexistent pid should yield ErrIdentityChanged, got %v", err)
	}
}

package teardownreq

import (
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
	dropped := Drop([]proctree.Process{old, recycled})
	if len(dropped) != 1 || dropped[0] != recycled {
		t.Fatalf("Drop must keep the recycled identity, got %v", dropped)
	}
}

// Drop returns the input untouched when nothing is registered — teardowns
// with no requester in the tree must not allocate or alter the set.
func TestDropLeavesUnregisteredSetAlone(t *testing.T) {
	in := []proctree.Process{proc(1, 1), proc(2, 2)}
	got := Drop(in)
	if len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("Drop must return the set unchanged, got %v", got)
	}
	if &got[0] != &in[0] {
		t.Fatal("Drop must return the same backing array when nothing dropped")
	}
}

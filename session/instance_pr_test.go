package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
)

// TestPRInfoWriteRollbackLeavesNoTrace pins the rollback contract (#3287
// review): a write undone because its durable half failed must restore the
// value, the freshness clock, AND the generation. A generation left advanced
// spuriously fails a concurrent producer's CAS — it discards a still-valid
// result even though nothing newer was committed — and a refreshed clock keeps
// the old value looking fresh for another staleness window.
func TestPRInfoWriteRollbackLeavesNoTrace(t *testing.T) {
	inst := &Instance{}
	inst.SetPRInfo(&git.PRInfo{Number: 1, State: "OPEN", Branch: "af/x"})
	genBefore := inst.PRInfoGeneration()
	ageBefore := inst.PRInfoAge()

	rollback := inst.BeginPRInfoWrite(&git.PRInfo{Number: 2, State: "MERGED", Branch: "af/x"})
	if got := inst.GetPRInfo(); got == nil || got.Number != 2 {
		t.Fatalf("BeginPRInfoWrite must apply the new value, got %+v", got)
	}
	if inst.PRInfoGeneration() == genBefore {
		t.Fatal("BeginPRInfoWrite must advance the generation like SetPRInfo")
	}

	inst.RollbackPRInfoWrite(rollback)
	if got := inst.GetPRInfo(); got == nil || got.Number != 1 {
		t.Fatalf("rollback must reinstate the previous value, got %+v", got)
	}
	if got := inst.PRInfoGeneration(); got != genBefore {
		t.Fatalf("rollback must reinstate the generation: got %d, want %d — an advanced generation fails a concurrent CAS for nothing", got, genBefore)
	}
	if inst.PRInfoAge() < ageBefore {
		t.Fatal("rollback must not refresh the freshness clock — a failed write earns no staleness window")
	}
}

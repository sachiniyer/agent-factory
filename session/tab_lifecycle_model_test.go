package session

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// A model-based check of the create-intent bookkeeping (#6030). The hand-written
// interleavings kept missing the next case, so this drives a random sequence
// over a small alphabet — create with or without an id, shape A or B, outcome
// success / definitive refusal / lost answer, sweep, close — against the fake
// sandbox and checks the invariants after every step:
//
//   - nothing executes twice: a create either rejoins an execution whose outcome
//     the daemon never saw, or (only when none exists) starts a new one;
//   - the daemon never holds a tab the sandbox lacks, and never twice;
//   - after a final sweep, every execution the sandbox still holds is on the
//     daemon roster exactly once.
//
// The seeds are fixed, so a failure names the seed and replays.
func TestCreateIntentModel(t *testing.T) {
	const seeds, depth = 150, 14
	shapes := []string{"A.sh", "B.sh"}
	ids := []string{"x", "y"}
	for seed := int64(1); seed <= seeds; seed++ {
		runIntentModel(t, seed, depth, shapes, ids)
		if t.Failed() {
			t.Fatalf("model failed at seed %d", seed)
		}
	}
}

func runIntentModel(t *testing.T, seed int64, depth int, shapes, ids []string) {
	rng := rand.New(rand.NewSource(seed))
	sb, inst := newLifecycleSandbox(t)
	// unresolved[shape] is the set of executions of that shape whose outcome the
	// daemon has not yet seen answered: a create of the shape must rejoin one.
	unresolved := map[string]map[string]bool{}
	for _, s := range shapes {
		unresolved[s] = map[string]bool{}
	}
	var trace []string
	fail := func(format string, args ...any) {
		t.Helper()
		require.FailNowf(t, "invariant broken", "seed %d after %v: %s", seed, trace, fmt.Sprintf(format, args...))
	}
	totalExecs := func() (n int) {
		for _, s := range shapes {
			n += sb.execs(s)
		}
		return n
	}
	forget := func(id string) {
		for _, s := range shapes {
			delete(unresolved[s], id)
		}
	}

	for step := 0; step < depth; step++ {
		switch op := rng.Intn(10); {
		case op < 6: // create
			shape := shapes[rng.Intn(len(shapes))]
			id := ""
			if rng.Intn(2) == 0 {
				id = ids[rng.Intn(len(ids))]
			}
			outcome := rng.Intn(3) // 0 success, 1 definitive refusal, 2 lost answer
			sb.mu.Lock()
			sb.dropCreate, sb.refuseNew = outcome == 2, outcome == 1
			held, heldHere := sb.tabs[id]
			before := map[string]bool{}
			for k := range sb.tabs {
				before[k] = true
			}
			sb.mu.Unlock()
			execsBefore := totalExecs()

			tab, err := inst.AddProcessTabWithID(id, shape, "")
			trace = append(trace, fmt.Sprintf("create(id=%q,%s,out=%d)", id, shape, outcome))

			// What the sandbox must have done: an id it already holds is
			// idempotent (or refused when another command owns it); an id-less
			// create rejoins an unresolved execution of its shape; only otherwise
			// does a command start.
			wantNew := 0
			switch {
			case id != "" && heldHere:
			case id == "" && len(unresolved[shape]) > 0:
			case outcome != 1:
				wantNew = 1
			}
			_ = held
			if got := totalExecs() - execsBefore; got != wantNew {
				fail("executions grew by %d, want %d (err=%v)", got, wantNew, err)
			}
			sb.mu.Lock()
			for k, td := range sb.tabs {
				if !before[k] {
					unresolved[td.Command][k] = true
				}
			}
			sb.mu.Unlock()
			if err == nil {
				require.NotNil(t, tab)
				forget(tab.ID)
			}
		case op < 8: // sweep
			trace = append(trace, "sweep")
			inst.ReconcileOffBoxTabs()
		default: // close a rostered tab
			ids := sortedKeys(rosterIDs(inst))
			if len(ids) == 0 {
				continue
			}
			id := ids[rng.Intn(len(ids))]
			trace = append(trace, "close("+id+")")
			if err := inst.CloseTabByID(id); err != nil {
				fail("close %s: %v", id, err)
			}
			forget(id)
		}

		// Every step: the daemon roster is a subset of the sandbox's.
		inSandbox := sb.ids()
		for id := range rosterIDs(inst) {
			if !inSandbox[id] {
				fail("daemon holds %q the sandbox lacks", id)
			}
		}
		inst.mu.RLock()
		seen := map[string]bool{}
		for _, tab := range inst.Tabs[1:] {
			if seen[tab.ID] {
				inst.mu.RUnlock()
				fail("daemon roster holds %q twice", tab.ID)
			}
			seen[tab.ID] = true
		}
		inst.mu.RUnlock()
	}

	sb.mu.Lock()
	sb.dropCreate, sb.refuseNew = false, false
	sb.mu.Unlock()
	inst.ReconcileOffBoxTabs()
	if got, want := rosterIDs(inst), sb.ids(); fmt.Sprint(sortedKeys(got)) != fmt.Sprint(sortedKeys(want)) {
		fail("after a final sweep daemon roster %v != sandbox %v", sortedKeys(got), sortedKeys(want))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for a := 1; a < len(out); a++ {
		for b := a; b > 0 && out[b] < out[b-1]; b-- {
			out[b], out[b-1] = out[b-1], out[b]
		}
	}
	return out
}

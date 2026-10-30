package session

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A model-based check of the create-intent bookkeeping (#6030). The hand-written
// interleavings kept missing the next case, so this drives a random sequence
// over a small alphabet — create with or without an id, shape A or B, outcome
// success / definitive refusal / lost answer, sweep, a replaced sandbox, intent expiry, a daemon-side tab taking a
// name a create holds, close (also with the
// daemon's persist failing) — against the fake
// sandbox and checks the invariants after every step:
//
//   - nothing executes twice: a create either rejoins an execution whose outcome
//     the daemon never saw, or (only when none exists) starts a new one;
//   - the daemon never holds a tab the sandbox lacks, and never twice;
//   - tab names are unique across the whole roster, adopted tabs included;
//   - a step whose commit fails leaves the daemon state, intents included,
//     exactly as it was;
//   - after a final sweep, every execution the sandbox still holds is on the
//     daemon roster exactly once.
//
// The seeds are fixed, so a failure names the seed and replays.
func TestCreateIntentModel(t *testing.T) {
	const seeds, depth = 300, 18
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
	local := map[string]bool{} // daemon-only tabs the sandbox never hears of
	localN := 0
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
		switch op := rng.Intn(13); {
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
			} else if outcome == 2 && id != "" && heldHere && held.Command == shape {
				// A lost answer for an id the sandbox already runs re-records the
				// intent: it is unresolved again, and joinable.
				unresolved[shape][id] = true
			}
		case op < 8: // sweep
			trace = append(trace, "sweep")
			inst.ReconcileOffBoxTabs()
		case op == 8: // every replay window runs out (#6023)
			trace = append(trace, "expire")
			inst.mu.Lock()
			for _, in := range inst.tabCreateIntents {
				in.at = time.Now().Add(-tabIntentTTL - time.Second)
			}
			inst.mu.Unlock()
			// No retry rejoins an execution any more; the sweep still owes each
			// unresolved one a place on the roster, which the final check holds.
			for _, s := range shapes {
				unresolved[s] = map[string]bool{}
			}
		case op == 12: // the sandbox is replaced (archive → restore): a fresh one holds nothing
			trace = append(trace, "replace-sandbox")
			sb.mu.Lock()
			sb.tabs = map[string]TabData{}
			sb.mu.Unlock()
			for _, s := range shapes {
				unresolved[s] = map[string]bool{}
			}
			// A restore outlasts the replay window, and the sweep's read of the new
			// sandbox's roster proves the executions it never held are gone.
			inst.mu.Lock()
			for _, in := range inst.tabCreateIntents {
				in.at = time.Now().Add(-tabIntentTTL - time.Second)
			}
			inst.mu.Unlock()
			inst.ReconcileOffBoxTabs()
		case op == 9: // a daemon-side tab takes a name a create may already hold (#5962)
			shape := shapes[rng.Intn(len(shapes))]
			trace = append(trace, "web("+shape+")")
			localN++
			id := fmt.Sprintf("web-%d", localN)
			local[id] = true
			inst.mu.Lock()
			inst.Tabs = append(inst.Tabs, &Tab{ID: id, Name: uniqueTabNameReserving(inst.Tabs, nil, processTabBaseName("", shape)), Kind: TabKindWeb, URL: "http://example.invalid"})
			inst.mu.Unlock()
		default: // close a rostered tab
			ids := sortedKeys(withoutLocal(rosterIDs(inst), local))
			if len(ids) == 0 {
				continue
			}
			id := ids[rng.Intn(len(ids))]
			switch mode := rng.Intn(3); mode {
			case 2: // the daemon's own persist fails: the step must change nothing
				before := intentStateSnapshot(inst)
				trace = append(trace, "close("+id+",commit-fails)")
				res, err := inst.CloseTabByIDWithCommit(id, func(InstanceData) error { return errCommitFailed })
				if err == nil || res.TeardownErr != nil {
					fail("a failed commit must fail the close (err=%v)", err)
				}
				if after := intentStateSnapshot(inst); !reflect.DeepEqual(before, after) {
					fail("a failed commit changed state:\n before %+v\n after  %+v", before, after)
				}
				continue
			case 1:
				trace = append(trace, "close("+id+",commit)")
				if _, err := inst.CloseTabByIDWithCommit(id, func(InstanceData) error { return nil }); err != nil {
					fail("close %s: %v", id, err)
				}
			default:
				trace = append(trace, "close("+id+")")
				if err := inst.CloseTabByID(id); err != nil {
					fail("close %s: %v", id, err)
				}
			}
			forget(id)
		}

		// Every step: the daemon roster is a subset of the sandbox's.
		inSandbox := sb.ids()
		for id := range withoutLocal(rosterIDs(inst), local) {
			if !inSandbox[id] {
				fail("daemon holds %q the sandbox lacks", id)
			}
		}
		inst.mu.RLock()
		seen := map[string]bool{}
		names := map[string]bool{}
		for _, tab := range inst.Tabs {
			if tab.ID != "" && seen[tab.ID] {
				inst.mu.RUnlock()
				fail("daemon roster holds %q twice", tab.ID)
			}
			seen[tab.ID] = true
			if names[tab.Name] {
				inst.mu.RUnlock()
				fail("two tabs are named %q", tab.Name)
			}
			names[tab.Name] = true
		}
		inst.mu.RUnlock()
	}

	sb.mu.Lock()
	sb.dropCreate, sb.refuseNew = false, false
	sb.mu.Unlock()
	inst.ReconcileOffBoxTabs()
	if got, want := withoutLocal(rosterIDs(inst), local), sb.ids(); fmt.Sprint(sortedKeys(got)) != fmt.Sprint(sortedKeys(want)) {
		fail("after a final sweep daemon roster %v != sandbox %v", sortedKeys(got), sortedKeys(want))
	}
}

var errCommitFailed = errors.New("persist failed")

// intentStateSnapshot is everything a step on the daemon's side can change: the
// roster, the create intents, and both pending-cleanup lists.
type intentSnapshot struct {
	Tabs     []string
	Intents  map[string]bool // key → adopted
	Closes   []RemoteTabCleanupData
	Cleanups []TabCleanupData
}

func intentStateSnapshot(i *Instance) intentSnapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	snap := intentSnapshot{Intents: map[string]bool{}}
	for _, tab := range i.Tabs {
		snap.Tabs = append(snap.Tabs, tab.ID+"/"+tab.gen+"/"+tab.Name)
	}
	for key, in := range i.tabCreateIntents {
		snap.Intents[key] = in.adopted
	}
	snap.Closes = append(snap.Closes, i.pendingRemoteClose...)
	snap.Cleanups = append(snap.Cleanups, i.pendingTabCleanup...)
	return snap
}

func withoutLocal(ids, local map[string]bool) map[string]bool {
	out := map[string]bool{}
	for id := range ids {
		if !local[id] {
			out[id] = true
		}
	}
	return out
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

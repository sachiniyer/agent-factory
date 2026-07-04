package store

import (
	"sort"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// fixed, deterministic timestamps (no time.Now nondeterminism in assertions).
var (
	tOldest = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tMid    = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	tNewer  = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	tNewest = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

func inst(title string, created time.Time) *session.Instance {
	return &session.Instance{Title: title, CreatedAt: created}
}

func titles(instances []*session.Instance) []string {
	out := make([]string, len(instances))
	for i, in := range instances {
		out[i] = in.Title
	}
	return out
}

func sortCopy(instances []*session.Instance) []*session.Instance {
	out := append([]*session.Instance(nil), instances...)
	sort.SliceStable(out, func(i, j int) bool { return LessInstanceOrder(out[i], out[j]) })
	return out
}

func assertOrder(t *testing.T, got []*session.Instance, want []string) {
	t.Helper()
	gotTitles := titles(got)
	if len(gotTitles) != len(want) {
		t.Fatalf("order length = %v, want %v", gotTitles, want)
	}
	for i := range want {
		if gotTitles[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotTitles, want)
		}
	}
}

// Root pins first even when it is the NEWEST instance: the pin is by reserved
// identity (#1106), not by being oldest.
func TestLessInstanceOrder_RootPinsDespiteNewestCreatedAt(t *testing.T) {
	in := []*session.Instance{
		inst("bravo", tMid),
		inst("root", tNewest), // newest — but reserved, so it must lead
		inst("alpha", tOldest),
	}
	assertOrder(t, sortCopy(in), []string{"root", "alpha", "bravo"})
}

// Non-root instances order oldest-first by CreatedAt.
func TestLessInstanceOrder_NonRootOldestFirst(t *testing.T) {
	in := []*session.Instance{
		inst("c", tNewest),
		inst("a", tOldest),
		inst("b", tMid),
	}
	assertOrder(t, sortCopy(in), []string{"a", "b", "c"})
}

// Equal CreatedAt breaks by Title, giving a total (and stable) order.
func TestLessInstanceOrder_TitleTiebreakOnEqualCreatedAt(t *testing.T) {
	in := []*session.Instance{
		inst("charlie", tMid),
		inst("alpha", tMid),
		inst("bravo", tMid),
	}
	assertOrder(t, sortCopy(in), []string{"alpha", "bravo", "charlie"})
}

// The reserved predicate is case-insensitive (session.IsReservedTitle), so a
// "Root"/" ROOT " row still pins.
func TestLessInstanceOrder_RootPinCaseInsensitive(t *testing.T) {
	in := []*session.Instance{
		inst("alpha", tOldest),
		inst(" ROOT ", tNewest),
	}
	assertOrder(t, sortCopy(in), []string{" ROOT ", "alpha"})
}

// Sorting is idempotent: an already-sorted slice does not change, so two
// identical snapshots produce identical order (no jitter across poll ticks).
func TestLessInstanceOrder_StableAcrossIdenticalSnapshots(t *testing.T) {
	build := func() []*session.Instance {
		// arbitrary input order
		return []*session.Instance{
			inst("delta", tNewest),
			inst("root", tMid),
			inst("alpha", tOldest),
			inst("bravo", tNewer),
		}
	}
	first := sortCopy(build())
	want := []string{"root", "alpha", "bravo", "delta"}
	assertOrder(t, first, want)

	// A second identical poll sorts to the same order...
	second := sortCopy(build())
	assertOrder(t, second, want)

	// ...and re-sorting the already-sorted slice is a no-op.
	again := sortCopy(first)
	assertOrder(t, again, want)
}

// A Lost root still pins top; a Lost non-root sorts by CreatedAt like any other
// instance (Lost is not special to ordering, #1108).
func TestLessInstanceOrder_LostStatusDoesNotAffectOrder(t *testing.T) {
	lostRoot := &session.Instance{Title: "root", CreatedAt: tNewest, Status: session.Lost}
	lostBravo := &session.Instance{Title: "bravo", CreatedAt: tMid, Status: session.Lost}
	in := []*session.Instance{
		lostBravo,
		inst("charlie", tNewer),
		lostRoot,
		inst("alpha", tOldest),
	}
	assertOrder(t, sortCopy(in), []string{"root", "alpha", "bravo", "charlie"})
}

// Projection integration: GetInstances() returns root-first-then-CreatedAt
// regardless of the order rows were added in.
func TestProjection_GetInstancesSortedOnAdd(t *testing.T) {
	p := NewProjection()
	for _, in := range []*session.Instance{
		inst("delta", tNewest),
		inst("root", tMid),
		inst("alpha", tOldest),
		inst("bravo", tNewer),
	} {
		p.AddInstance(in) // finalizer intentionally not run (no repo/worktree)
	}
	assertOrder(t, p.GetInstances(), []string{"root", "alpha", "bravo", "delta"})
}

// A root re-ensure (#1108 Lost → recreate) rebuilds root with a NEWER CreatedAt.
// The pin is by identity, so ReplaceInstance keeps root at index 0.
func TestProjection_RootPinSurvivesReEnsure(t *testing.T) {
	p := NewProjection()
	oldRoot := inst("root", tOldest)
	p.AddInstance(oldRoot)
	p.AddInstance(inst("alpha", tMid))
	p.AddInstance(inst("bravo", tNewer))
	assertOrder(t, p.GetInstances(), []string{"root", "alpha", "bravo"})

	// Recreated root carries a brand-new (newest) CreatedAt.
	newRoot := inst("root", tNewest)
	if !p.ReplaceInstance(oldRoot, newRoot) {
		t.Fatalf("ReplaceInstance did not find the old root")
	}
	got := p.GetInstances()
	assertOrder(t, got, []string{"root", "alpha", "bravo"})
	if got[0] != newRoot {
		t.Fatalf("root row = %p, want the rebuilt root %p", got[0], newRoot)
	}
}

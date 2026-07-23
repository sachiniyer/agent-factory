package session

import (
	"fmt"
	"sync"
	"testing"
)

// TestAttachVSCodeTabIDAdoptDoesNotRaceSnapshot is the #2420 regression guard.
//
// resolveAttachedTabLocked adopts a daemon-minted id into an id-less local tab
// projection. GetTabs hands out the LIVE *Tab pointers (it copies only the
// slice), and callers — tree.TabLabels on the render path, the pane refresh —
// read Name/Kind/ID off-lock. The instance.go copy-on-write invariant (the
// GetTabs doc, established by #1930) therefore forbids assigning those fields in
// place once a tab is observable via i.Tabs: a writer must swap in a COPY via
// replaceTabFieldLocked so an existing snapshot keeps reading stable data.
//
// The prior `tab.ID = tabID` in resolveAttachedTabLocked violated that: it wrote
// the id field of a pointer a GetTabs snapshot still held, off-lock readers and
// the locked writer touching the same field with no happens-before edge — a data
// race, not a stale read.
//
// Run under -race: each of the N id-less tabs takes exactly one adopt write
// while a reader reads that tab's snapshot pointer's ID off-lock. The buggy
// in-place write fails the detector on those N independent field races; the
// copy-on-write fix leaves every snapshot pointer untouched and passes.
func TestAttachVSCodeTabIDAdoptDoesNotRaceSnapshot(t *testing.T) {
	const n = 256

	i := &Instance{}
	i.Tabs = make([]*Tab, n)
	for k := range i.Tabs {
		i.Tabs[k] = &Tab{Name: fmt.Sprintf("editor-%d", k)}
	}

	// A snapshot taken before the daemon ids arrive: live pointers whose ID a
	// renderer reads without holding i.mu.
	snapshot := i.GetTabs()

	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: off-lock reads of every handed-out pointer's ID. The accumulator
	// keeps the reads from being optimized away.
	var observed int
	go func() {
		defer wg.Done()
		for round := 0; round < 64; round++ {
			for k := range snapshot {
				observed += len(snapshot[k].ID)
			}
		}
	}()

	// Writer: adopt a distinct daemon id into each id-less tab through the real
	// public attach path (which resolves via resolveAttachedTabLocked).
	go func() {
		defer wg.Done()
		for k := 0; k < n; k++ {
			tab, err := i.AttachVSCodeTab(fmt.Sprintf("editor-%d", k), fmt.Sprintf("daemon-%d", k))
			if err != nil {
				t.Errorf("AttachVSCodeTab adopt failed for editor-%d: %v", k, err)
				return
			}
			if tab.ID != fmt.Sprintf("daemon-%d", k) {
				t.Errorf("adopt returned id %q, want %q", tab.ID, fmt.Sprintf("daemon-%d", k))
				return
			}
		}
	}()

	wg.Wait()
	_ = observed

	// Functional check: the adopt is observable on the authoritative list, so the
	// copy-on-write path is not a silent no-op.
	final := i.GetTabs()
	for k := 0; k < n; k++ {
		if got, want := final[k].ID, fmt.Sprintf("daemon-%d", k); got != want {
			t.Fatalf("tab editor-%d id = %q, want %q", k, got, want)
		}
	}
	// The pre-adopt snapshot pointers must still read their original id-less
	// value: copy-on-write means the adopt swapped in new pointers rather than
	// mutating the ones the snapshot handed out.
	for k := 0; k < n; k++ {
		if snapshot[k].ID != "" {
			t.Fatalf("snapshot pointer for editor-%d was mutated in place: id = %q", k, snapshot[k].ID)
		}
	}
}

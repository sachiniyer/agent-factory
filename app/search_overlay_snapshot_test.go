package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShowSearchOverlay_StableAcrossSnapshotRemoval is the #1008 regression.
//
// showSearchOverlay must hand the overlay a stable COPY of the instance list.
// Sidebar.GetInstances() returns a slice sharing the sidebar's backing array,
// and a background snapshotFetchedMsg reconcile removes instances in place via
// append(s.instances[:i], s.instances[i+1:]...). If the overlay retained that
// shared header, the in-place shift would corrupt the slice it re-scans on the
// next keystroke — a duplicated trailing entry plus a "ghost" (the removed
// head dropped off the tail). Opening from GetInstancesSnapshot() gives the
// overlay a private backing array, so a later removal leaves its list untouched.
//
// The corruption only surfaces when updateResults() re-runs (on a keystroke)
// AFTER the mutation, so the test types a query that matches every session.
//
// Fail-before/pass-after: with the buggy GetInstances() call, after "xa" is
// removed the overlay re-scans the corrupted backing array ["xb","xc","xc"]
// (dup "xc", ghost: "xa" gone). With the snapshot copy it stays
// ["xa","xb","xc"].
func TestShowSearchOverlay_StableAcrossSnapshotRemoval(t *testing.T) {
	h := newTestHome(t)

	// Titles share the rune 'x' so a single-key query matches all of them,
	// forcing updateResults to re-scan the (possibly corrupted) backing array.
	xa := newSnapshotTestInstance(t, "xa")
	xb := newSnapshotTestInstance(t, "xb")
	xc := newSnapshotTestInstance(t, "xc")
	h.sidebar.AddInstance(xa)
	h.sidebar.AddInstance(xb)
	h.sidebar.AddInstance(xc)

	// Open the overlay exactly as the key handler does.
	_, _ = h.showSearchOverlay()
	require.NotNil(t, h.searchOverlay, "search overlay must open with sessions present")
	require.Equal(t, stateSearch, h.state)

	// A background reconcile drops "xa" (the head element, so its removal
	// append-shifts the sidebar's backing array under the overlay).
	removed := h.reconcileSnapshot([]session.InstanceData{
		xb.ToInstanceData(),
		xc.ToInstanceData(),
	})
	require.True(t, removed, "dropping a session from the snapshot is a change")
	assert.Nil(t, findSidebarInstance(h, "xa"), "reconcile must remove 'xa' from the sidebar")

	// User types 'x' — every session matches, so updateResults re-scans the
	// overlay's whole list. A shared backing array would now read corrupted.
	h.searchOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	// The overlay was opened from a stable copy, so its list is unaffected by
	// the sidebar removal: no duplicate, no ghost-induced drop.
	titles := overlayResultTitles(h)
	assert.Equal(t, []string{"xa", "xb", "xc"}, titles,
		"overlay list must stay the stable copy captured at open time")

	// Explicit no-duplicate assertion mirroring the bug symptom.
	seen := map[string]int{}
	for _, name := range titles {
		seen[name]++
	}
	for name, n := range seen {
		assert.Equalf(t, 1, n, "title %q must appear exactly once (no duplicates)", name)
	}
}

func overlayResultTitles(h *home) []string {
	insts := h.searchOverlay.ResultInstances()
	titles := make([]string, len(insts))
	for i, inst := range insts {
		titles[i] = inst.Title
	}
	return titles
}

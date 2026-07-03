package layout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// paneA/paneB are two pane region ids for the generic ring tests; the app
// keys ring entries on store pane ids via layout.PaneRegion.
var (
	paneA = layout.PaneRegion(1)
	paneB = layout.PaneRegion(2)
)

// newWorkspaceRing builds the canonical §2.3 ring:
// tree → pane 1 → pane 2 → automations.
func newWorkspaceRing() *layout.Ring {
	return layout.NewRing(layout.RegionTree, paneA, paneB, layout.RegionAutomations)
}

func TestRingCyclingOrder(t *testing.T) {
	r := newWorkspaceRing()
	assert.Equal(t, layout.RegionTree, r.Active(), "first id starts active")

	assert.Equal(t, paneA, r.Next())
	assert.Equal(t, paneB, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Next())
	assert.Equal(t, layout.RegionTree, r.Next(), "Next wraps around")

	assert.Equal(t, layout.RegionAutomations, r.Prev(), "Prev wraps around backwards")
	assert.Equal(t, paneB, r.Prev())
	assert.Equal(t, paneA, r.Prev())
	assert.Equal(t, layout.RegionTree, r.Prev())
}

func TestRingSkipsHidden(t *testing.T) {
	r := newWorkspaceRing()
	// No split: pane B leaves the ring.
	r.SetHidden(paneB, true)

	assert.Equal(t, paneA, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Next(), "hidden pane B is skipped forward")
	assert.Equal(t, layout.RegionTree, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Prev(), "hidden pane B is skipped backward")

	// Split reopens: pane B rejoins the ring in place.
	r.SetHidden(paneB, false)
	require.True(t, r.Focus(paneA))
	assert.Equal(t, paneB, r.Next())
}

func TestRingFocus(t *testing.T) {
	r := newWorkspaceRing()

	require.True(t, r.Focus(layout.RegionAutomations))
	assert.Equal(t, layout.RegionAutomations, r.Active())
	assert.Equal(t, layout.RegionTree, r.Next(), "cycling continues from the focused id")

	assert.False(t, r.Focus("no-such-region"), "unknown id refused")
	assert.Equal(t, layout.RegionTree, r.Active(), "failed Focus leaves focus unchanged")

	r.SetHidden(paneB, true)
	assert.False(t, r.Focus(paneB), "hidden id refused")
	assert.Equal(t, layout.RegionTree, r.Active())
}

func TestRingHidingActiveAdvances(t *testing.T) {
	r := newWorkspaceRing()
	require.True(t, r.Focus(paneB))

	// The split closes while pane B holds focus: focus moves to the next
	// visible id in ring order.
	r.SetHidden(paneB, true)
	assert.Equal(t, layout.RegionAutomations, r.Active())
}

func TestRingAllHidden(t *testing.T) {
	r := newWorkspaceRing()
	for _, id := range []string{layout.RegionTree, paneA, paneB, layout.RegionAutomations} {
		r.SetHidden(id, true)
	}
	assert.Equal(t, "", r.Active())
	assert.Equal(t, "", r.Next())
	assert.Equal(t, "", r.Prev())
	assert.False(t, r.Focus(layout.RegionTree))

	// Everything reappears: the ring recovers.
	r.SetHidden(layout.RegionTree, false)
	assert.Equal(t, layout.RegionTree, r.Active())
}

func TestRingSingleVisible(t *testing.T) {
	r := newWorkspaceRing()
	r.SetHidden(paneA, true)
	r.SetHidden(paneB, true)
	r.SetHidden(layout.RegionAutomations, true)

	assert.Equal(t, layout.RegionTree, r.Active())
	assert.Equal(t, layout.RegionTree, r.Next(), "Next on a single visible id stays put")
	assert.Equal(t, layout.RegionTree, r.Prev(), "Prev on a single visible id stays put")
}

func TestRingEmpty(t *testing.T) {
	r := layout.NewRing()
	assert.Equal(t, "", r.Active())
	assert.Equal(t, "", r.Next())
	assert.Equal(t, "", r.Prev())
	assert.False(t, r.Focus(layout.RegionTree))
}

// TestRingSetIDs pins the dynamic-ring contract (#1088): SetIDs reshapes the
// ring as panes open/close, keeping the active id when it survives, falling
// back to the first id when it doesn't, carrying hidden flags for surviving
// ids and forgetting them for dropped ones.
func TestRingSetIDs(t *testing.T) {
	r := newWorkspaceRing()
	require.True(t, r.Focus(paneB))

	// A pane opens: the active id survives in the reshaped ring.
	paneC := layout.PaneRegion(3)
	r.SetIDs(layout.RegionTree, paneA, paneB, paneC, layout.RegionAutomations)
	assert.Equal(t, paneB, r.Active(), "active id survives SetIDs")
	assert.Equal(t, paneC, r.Next(), "cycling follows the new order")

	// The active pane closes: focus falls back to the first id.
	r.SetIDs(layout.RegionTree, paneA, paneB, layout.RegionAutomations)
	assert.Equal(t, layout.RegionTree, r.Active(), "vanished active falls back to the first id")

	// Hidden flags: surviving ids keep them, dropped ids forget them.
	r.SetHidden(layout.RegionAutomations, true)
	r.SetHidden(paneB, true)
	r.SetIDs(layout.RegionTree, paneA, layout.RegionAutomations)
	require.True(t, r.Focus(paneA))
	assert.Equal(t, layout.RegionTree, r.Next(), "hidden automations still skipped after SetIDs")
	r.SetIDs(layout.RegionTree, paneA, paneB, layout.RegionAutomations)
	require.True(t, r.Focus(paneB), "a dropped-and-readded id starts visible again")
}

package layout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// newWorkspaceRing builds the canonical §2.3 ring:
// tree → pane A → pane B → automations.
func newWorkspaceRing() *layout.Ring {
	return layout.NewRing(layout.RegionTree, layout.RegionPaneA, layout.RegionPaneB, layout.RegionAutomations)
}

func TestRingCyclingOrder(t *testing.T) {
	r := newWorkspaceRing()
	assert.Equal(t, layout.RegionTree, r.Active(), "first id starts active")

	assert.Equal(t, layout.RegionPaneA, r.Next())
	assert.Equal(t, layout.RegionPaneB, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Next())
	assert.Equal(t, layout.RegionTree, r.Next(), "Next wraps around")

	assert.Equal(t, layout.RegionAutomations, r.Prev(), "Prev wraps around backwards")
	assert.Equal(t, layout.RegionPaneB, r.Prev())
	assert.Equal(t, layout.RegionPaneA, r.Prev())
	assert.Equal(t, layout.RegionTree, r.Prev())
}

func TestRingSkipsHidden(t *testing.T) {
	r := newWorkspaceRing()
	// No split: pane B leaves the ring.
	r.SetHidden(layout.RegionPaneB, true)

	assert.Equal(t, layout.RegionPaneA, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Next(), "hidden pane B is skipped forward")
	assert.Equal(t, layout.RegionTree, r.Next())
	assert.Equal(t, layout.RegionAutomations, r.Prev(), "hidden pane B is skipped backward")

	// Split reopens: pane B rejoins the ring in place.
	r.SetHidden(layout.RegionPaneB, false)
	require.True(t, r.Focus(layout.RegionPaneA))
	assert.Equal(t, layout.RegionPaneB, r.Next())
}

func TestRingFocus(t *testing.T) {
	r := newWorkspaceRing()

	require.True(t, r.Focus(layout.RegionAutomations))
	assert.Equal(t, layout.RegionAutomations, r.Active())
	assert.Equal(t, layout.RegionTree, r.Next(), "cycling continues from the focused id")

	assert.False(t, r.Focus("no-such-region"), "unknown id refused")
	assert.Equal(t, layout.RegionTree, r.Active(), "failed Focus leaves focus unchanged")

	r.SetHidden(layout.RegionPaneB, true)
	assert.False(t, r.Focus(layout.RegionPaneB), "hidden id refused")
	assert.Equal(t, layout.RegionTree, r.Active())
}

func TestRingHidingActiveAdvances(t *testing.T) {
	r := newWorkspaceRing()
	require.True(t, r.Focus(layout.RegionPaneB))

	// The split closes while pane B holds focus: focus moves to the next
	// visible id in ring order.
	r.SetHidden(layout.RegionPaneB, true)
	assert.Equal(t, layout.RegionAutomations, r.Active())
}

func TestRingAllHidden(t *testing.T) {
	r := newWorkspaceRing()
	for _, id := range []string{layout.RegionTree, layout.RegionPaneA, layout.RegionPaneB, layout.RegionAutomations} {
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
	r.SetHidden(layout.RegionPaneA, true)
	r.SetHidden(layout.RegionPaneB, true)
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

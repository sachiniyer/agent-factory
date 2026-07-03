package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// TestTabbedWindowRegistersBodyAndHeaderZones (#1024 PR 6): the pane
// registers its whole rect as the body and the one-line header on top of it,
// with the header zone sitting on the row that actually renders the header
// text — under both the pane-A and pinned pane-B identities.
func TestTabbedWindowRegistersBodyAndHeaderZones(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
		region string
	}{
		{"paneA", false, layout.RegionPaneA},
		{"paneB", true, layout.RegionPaneB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tw := newTestTabbedWindow()
			if tc.pinned {
				tw = NewPinnedTabbedWindow(NewTabPane(), tw.proj)
			}
			reg := zones.NewRegistry()
			tw.SetZoneRegistry(reg)
			rect := layout.Rect{X: 30, Y: 2, W: 60, H: 20}
			tw.SetRect(rect)

			reg.Reset()
			out := tw.String()
			lines := plainLines(out)

			body, ok := reg.Find(zones.PaneBody(tc.region))
			require.True(t, ok, "body zone; got %v", reg.IDs())
			assert.Equal(t, rect, body, "the body zone is the whole pane rect")

			header, ok := reg.Find(zones.PaneHeader(tc.region))
			require.True(t, ok, "header zone")
			assert.Equal(t, layout.Rect{X: rect.X + 1, Y: rect.Y + 1, W: rect.W - 2, H: 1}, header,
				"the header sits inside the frame border")
			// The header zone's row renders the header text (nil binding here,
			// so the placeholder header).
			assert.Contains(t, lines[header.Y-rect.Y], "no session selected",
				"the header zone must sit on the rendered header line")

			// Precedence: a point on the header row resolves to the header, a
			// point below it to the body.
			id, _, ok := reg.Resolve(header.X+2, header.Y)
			require.True(t, ok)
			assert.Equal(t, zones.PaneHeader(tc.region), id)
			id, _, ok = reg.Resolve(header.X+2, header.Y+3)
			require.True(t, ok)
			assert.Equal(t, zones.PaneBody(tc.region), id)
		})
	}
}

// TestTabbedWindowNoZonesWhenHidden: a zero rect (pane B without a split)
// renders nothing and must register nothing.
func TestTabbedWindowNoZonesWhenHidden(t *testing.T) {
	tw := newTestTabbedWindow()
	reg := zones.NewRegistry()
	tw.SetZoneRegistry(reg)
	tw.SetRect(layout.Rect{})

	reg.Reset()
	_ = tw.String()
	assert.Empty(t, reg.IDs(), "a hidden pane must not register hit zones")
}

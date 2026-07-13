package app

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/require"
)

// fireIdlePreviewTick drives the real 100ms preview tick through Update, so the
// tests exercise the same inPreviewTick wiring the running TUI uses.
func fireIdlePreviewTick(h *home) {
	_, _ = h.Update(previewTickMsg{})
}

// TestPane_ForwardTabVisitsAllPanesDespiteIdleTick is the #1558 regression. In
// a three-pane workspace the forward focus ring must cycle
// tree → pane → pane → pane → automations → projects → tree, visiting every
// pane and resting on the tree — even though the 100ms preview tick keeps
// firing between keystrokes. Before the fix, that idle tick yanked focus back
// onto the selected instance's already-open pane the moment the user Tabbed off
// it, so the ring never reached the other panes or settled on the tree.
func TestPane_ForwardTabVisitsAllPanesDespiteIdleTick(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes())
	require.Equal(t, 3, h.lastLayout.PaneCount(), "200 cols fits three panes")
	panes := h.store.OpenPanes()
	// The selection rests on gamma (the last opened), whose pane is rightmost.
	h.focusRegion(layout.RegionTree)

	forward := []string{
		layout.PaneRegion(panes[0].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[2].ID()),
		layout.RegionAutomations,
		layout.RegionProjects,
		layout.RegionTree,
	}
	for _, want := range forward {
		_ = h.cycleFocus(false)
		fireIdlePreviewTick(h) // the idle tick fires between keystrokes in the running TUI
		require.Equal(t, want, h.ring.Active(),
			"forward Tab + idle preview tick must advance the ring, not steal focus to the selected pane")
	}

	// Reverse mirrors it and is likewise immune to the idle tick.
	backward := []string{
		layout.RegionProjects,
		layout.RegionAutomations,
		layout.PaneRegion(panes[2].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[0].ID()),
		layout.RegionTree,
	}
	for _, want := range backward {
		_ = h.cycleFocus(true)
		fireIdlePreviewTick(h)
		require.Equal(t, want, h.ring.Active(),
			"Shift-Tab + idle preview tick must cycle the ring backwards")
	}
}

// TestPane_IdleTickDoesNotStealFocusFromTree is the tree half of #1558: while
// the ring rests on the tree with the selected instance's pane open, the idle
// preview tick must NOT pull focus onto that pane. Focus stealing on every tick
// is what broke the af_focus_tree driver helper (it could never rest on the
// tree). A real navigation still focuses an already-open tab (#1493), covered by
// TestPanePreviewSelectionFocusesAlreadyOpenTabPane.
func TestPane_IdleTickDoesNotStealFocusFromTree(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active())

	for i := 0; i < 5; i++ {
		fireIdlePreviewTick(h)
		require.Equal(t, layout.RegionTree, h.ring.Active(),
			"the idle preview tick must leave focus on the tree, not steal it to the selected pane")
	}
}

// TestPane_SnapshotFetchDoesNotStealFocusFromOtherPane is the snapshot-poll twin
// of #1558 (#1603): a background daemon snapshot poll is a background refresh too,
// so the selectionChanged it fires on any out-of-band change (new/removed session,
// tab set changed) must NOT steal focus onto the selected instance's already-open
// pane. Before the fix, snapshotFetchedMsg ran selectionChanged ungated, so a
// snapshot arriving while the user was focused in another pane yanked focus back
// onto the selected instance's open pane.
//
// The message is driven through Update (not handleSnapshot directly) so the real
// snapshotFetchedMsg wiring runs exactly once — a second handleSnapshot on the
// same data would report changed==false and never reach the gated branch.
func TestPane_SnapshotFetchDoesNotStealFocusFromOtherPane(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	gamma := h.store.GetInstanceByTitle("gamma")

	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// Open alpha's agent tab in pane1.
	_ = openTestPane(t, h, alpha, 0)
	// Open beta's agent tab in pane2.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pane2 := openTestPane(t, h, beta, 0)

	// Select alpha (already open in pane1) — a user-driven selectionChanged, so
	// the open-or-focus verb (#1493) focuses pane1.
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()

	// The user then manually focuses pane2 (beta), away from the selection.
	h.focusRegion(layout.PaneRegion(pane2.ID()))
	require.Equal(t, layout.PaneRegion(pane2.ID()), h.ring.Active(),
		"user manually focused pane2 (beta)")
	require.Equal(t, "alpha", h.sidebar.GetSelectedInstance().Title,
		"selection rests on alpha (open in pane1)")

	// A background snapshot poll reports a new session "delta" — an out-of-band
	// change that reconciles to changed==true and fires selectionChanged.
	snap := snapshotFetchedMsg{
		data: []session.InstanceData{
			alpha.ToInstanceData(),
			beta.ToInstanceData(),
			gamma.ToInstanceData(),
			{Title: "delta", CreatedAt: time.Now()},
		},
		repoID: h.repoID,
	}
	_, _ = h.Update(snap)

	require.NotNil(t, h.store.GetInstanceByTitle("delta"),
		"the snapshot reconciled the new session (changed==true, gated branch taken)")
	require.Equal(t, layout.PaneRegion(pane2.ID()), h.ring.Active(),
		"background snapshot reconciliation must not steal focus from pane2 (beta) to pane1 (alpha)")
}

// TestPane_ShiftTabReverseTraversalDismissesLivePreview is the #1705 regression.
// A live pane preview must NOT swallow a focus-ring keystroke: Shift+Tab from a
// focused pane while a preview is active must dismiss the preview AND still step
// the ring back one stop (to the tree). Before the fix, cycleFocus early-returned
// after cancelling the preview, so the ring never moved — and because the idle
// preview tick creates a preview whenever the tree cursor names a tab the owner
// pane isn't bound to, reverse traversal out of a pane was effectively dead.
func TestPane_ShiftTabReverseTraversalDismissesLivePreview(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	setPreviewText(alpha, "alpha-body")
	beta := h.store.GetInstanceByTitle("beta")
	setPreviewText(beta, "beta-body")

	// Open a pane on alpha, then select beta so alpha's pane previews beta — a
	// live preview txn, exactly as the running TUI holds when the tree cursor
	// rests on a not-yet-open tab.
	pressKey(t, h, "s")
	pane := h.store.OpenPanes()[0]
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "a cross-instance selection creates a live preview")

	// Focus the pane, then Shift+Tab.
	h.focusRegion(layout.PaneRegion(pane.ID()))
	require.Equal(t, layout.PaneRegion(pane.ID()), h.ring.Active())
	pressTab(t, h, true)

	require.Nil(t, h.panePreviewTxn, "Shift+Tab dismisses the transient preview")
	require.Equal(t, layout.RegionTree, h.ring.Active(),
		"#1705: Shift+Tab reverse-traverses out of the pane to the tree, not swallowed")
}

// TestSectionNav_JumpsAcrossRailSections is the #1706 regression. `]` / `[`
// ("next / prev section") must reach the automations and projects rail sections,
// which moved out of the sidebar (#1470 / #1588 / #1590) into their own
// Tab-focusable regions. Before the fix the binding walked sidebar section
// headers only, so from the instances tree it was a silent no-op — Automations
// was unreachable via `]`.
func TestSectionNav_JumpsAcrossRailSections(t *testing.T) {
	h := paneTestHome(t)
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active())

	// `]` from an instance row reaches the Automations section (drops D kill).
	pressKey(t, h, "]")
	require.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"#1706: ] from the instances tree must reach the Automations section")

	// `]` again → Projects; again → wraps back to the tree.
	pressKey(t, h, "]")
	require.Equal(t, layout.RegionProjects, h.ring.Active())
	pressKey(t, h, "]")
	require.Equal(t, layout.RegionTree, h.ring.Active())

	// `[` walks back the other way.
	pressKey(t, h, "[")
	require.Equal(t, layout.RegionProjects, h.ring.Active(),
		"#1706: [ walks the sections in reverse")
}

// TestSectionNav_SkipsWorkspacePanes proves `]` jumps SECTION to SECTION, not
// through the open workspace panes (those are Tab's / 1-9's job): with a pane
// open, `]` from the tree lands on Automations, skipping the pane region.
func TestSectionNav_SkipsWorkspacePanes(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s") // open a pane on alpha
	require.Equal(t, 1, h.store.NumOpenPanes())
	h.focusRegion(layout.RegionTree)

	pressKey(t, h, "]")
	require.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"#1706: ] skips the workspace pane and lands on the next section")
}

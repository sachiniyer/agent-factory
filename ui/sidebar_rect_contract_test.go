package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// joinedFamily is the four-emoji, three-ZWJ family the width functions in this
// tree disagree about most: x/ansi and lipgloss say 2, PrintableRuneWidth says 8,
// and tmux 3.4 advances 4 (see layout.Cells' table).
const joinedFamily = "\U0001F468\u200d\U0001F469\u200d\U0001F467\u200d\U0001F466"

// readyDot and activeTabMarker are tree's own glyphs, spelled out here because
// they are unexported there. Pinned against the rendered output rather than
// imported, which is the point: the assertion is about what reaches the screen.
const (
	readyDot        = "\u25cf" // tree.readyIcon, without its trailing pad space
	activeTabMarker = " *"     // tree.activeTabMarker
)

// #3614, through the real sidebar rather than through the clamp alone. Both
// halves of the decision are asserted on ONE row, because either alone is a trap:
//
//   - the rendered rail must not be able to exceed its rectangle. Master's rail
//     did: the row for a clustered title measured 22 by every function in the tree
//     and drew 24 cells in tmux, so the pane beside it was pushed off the frame
//     and the row wrapped, making every height budget above it a lie (#3430).
//   - and it must still show its ● status dot. Bounding the row without laying
//     the glyph out from the edge just trades the overflow for a missing status
//     marker, which is the measured harm that made #3610 withdraw the overestimate
//     as the general measure — not an acceptable trade on the most-looked-at pane.
//
// 22 columns is the rail minimum (layout.TreeMinWidth, #1090), so this is the
// tightest supported width rather than a contrived one.
func TestSidebarRowCannotOverflowAndKeepsItsStatusDot(t *testing.T) {
	const width, height = 22, 18

	s := NewSidebar(store.NewProjection())
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fam" + joinedFamily + "zzz", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	addTestInstance(s, inst)
	s.SetSize(width, height)

	out := s.String()
	lines := strings.Split(out, "\n")
	require.Len(t, lines, height, "the rail is exactly its allocated height")

	var titleRow string
	for _, line := range lines {
		if strings.Contains(line, "fam") {
			titleRow = line
		}
		// The contract is one-sided for clustered content — a row may fall SHORT
		// of the rectangle — but no row may exceed it, which is what wraps.
		if got := layout.CellsUpperBound(line); got > width {
			t.Errorf("a rail row can occupy %d cells, past the %d-column rectangle: %q",
				got, width, xansi.Strip(line))
		}
	}
	require.NotEmpty(t, titleRow, "the clustered session's title row must be on screen")

	if !strings.Contains(titleRow, readyDot) {
		t.Errorf("the Ready session lost its %q status dot to the width accounting: %q",
			readyDot, xansi.Strip(titleRow))
	}
	// And the glyph is the row's right-hand column, not something that merely
	// survived somewhere in the middle.
	stripped := strings.TrimRight(xansi.Strip(titleRow), " ")
	if !strings.HasSuffix(stripped, readyDot) {
		t.Errorf("the status dot must be the row's trailing glyph, got %q", stripped)
	}
}

// The other half of the same promise, and the one that keeps the bound in its
// branch: an ordinary ASCII title renders byte-identically to before, filling the
// rectangle exactly. The upper bound is a deliberate overestimate, so any leak of
// it into ordinary rows shortens real titles for nothing.
func TestSidebarOrdinaryRowsFillTheRectangleExactly(t *testing.T) {
	const width, height = 22, 18

	s := NewSidebar(store.NewProjection())
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "ordinary-session", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	addTestInstance(s, inst)
	s.SetSize(width, height)

	for i, line := range strings.Split(s.String(), "\n") {
		if got := layout.Cells(line); got != width {
			t.Errorf("row %d is %d cells, want exactly %d: %q", i, got, width, xansi.Strip(line))
		}
	}
}

// A clustered tab label must not cost the row its " *" active marker either. The
// cue is the ONLY thing distinguishing an active tab (tabRowActiveStyle is
// deliberately identical to tabRowStyle, #1983), so losing it to width accounting
// makes an active tab byte-identical with an inactive one.
func TestSidebarTabRowKeepsItsActiveMarker(t *testing.T) {
	const width, height = 22, 18

	s := NewSidebar(store.NewProjection())
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "t" + joinedFamily, Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	addAgentShellTabs(inst)
	addTestInstance(s, inst)
	s.SetSize(width, height)
	s.proj.SetSelectedInstance(inst)
	s.syncFromStore()

	out := s.String()
	var tabRows int
	for _, line := range strings.Split(out, "\n") {
		if got := layout.CellsUpperBound(line); got > width {
			t.Errorf("a rail row can occupy %d cells, past the %d-column rectangle: %q",
				got, width, xansi.Strip(line))
		}
		if strings.ContainsAny(line, "├└") {
			tabRows++
		}
	}
	require.NotZero(t, tabRows, "the expanded session must render its tab child rows")
	require.Contains(t, xansi.Strip(out), activeTabMarker,
		"the active tab's marker must survive the width accounting")
}

// The property that actually reaches tmux, composed the way app's View does
// (app/home_view.go): rail over rule over automations, that beside the workspace
// pane, and the status bar under both.
//
// This is where #3614 would otherwise have been fixed on paper only. ClampToRect
// keeps a clustered row inside its rectangle by withholding cells it cannot
// account for — and lipgloss's Join, measuring the row with the very function
// that under-counts it, pads those cells straight back on the way into the frame.
// Measured: the row leaves the clamp at 22 and reaches the terminal at 24 again.
// So the assertion is on the COMPOSED frame, not on the pane.
func TestComposedFrameCannotOverflowWithAClusteredTitle(t *testing.T) {
	const termW, termH = 100, 30
	lay := layout.Grid{Panes: 1}.Solve(termW, termH)
	require.False(t, lay.Fallback)

	proj := store.NewProjection()
	sidebar := NewSidebar(proj)
	paneA := NewTabbedWindow(NewTabPane(previewFromInstance), nil)
	automations := NewAutomationsPane(proj)
	statusBar := NewStatusBar(NewMenu(), NewErrBox())

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "fam" + joinedFamily + "zzz", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	finalize := proj.AddInstance(inst)
	if finalize != nil {
		defer finalize()
	}
	sidebar.syncFromStore()

	sidebar.SetRect(lay.Tree)
	paneA.SetRect(lay.Panes[0])
	statusBar.SetRect(lay.StatusBar)
	rail := sidebar.View()
	if lay.AutomationsVisible {
		automations.SetRect(lay.Automations)
		automations.SetCompact(lay.AutomationsCompact)
		rail = layout.JoinVertical(rail, strings.Repeat("─", lay.RailRule.W), automations.View())
	}
	frame := layout.JoinVertical(layout.JoinHorizontal(rail, paneA.View()), statusBar.View())

	lines := strings.Split(frame, "\n")
	require.Len(t, lines, termH, "the composed frame is exactly the terminal height")
	clustered := 0
	for i, line := range lines {
		if got := layout.CellsUpperBound(line); got > termW {
			t.Errorf("frame row %d can occupy %d cells in a %d-column terminal — it "+
				"wraps, and every height budget above it becomes a lie (#3430): %q",
				i, got, termW, xansi.Strip(line))
		}
		if strings.Contains(line, "fam") {
			clustered++
			if !strings.Contains(line, readyDot) {
				t.Errorf("frame row %d lost the session's status dot: %q", i, xansi.Strip(line))
			}
		}
	}
	require.Equal(t, 1, clustered, "the clustered title must be on screen exactly once")
}

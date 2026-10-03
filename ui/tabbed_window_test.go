package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setWindowSize rects the pane at origin, the test shorthand for the layout
// engine's SetRect call.
func setWindowSize(w *TabbedWindow, width, height int) {
	w.SetRect(layout.Rect{W: width, H: height})
}

func startedWindowInstance(t *testing.T, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	addAgentShellTabs(inst)
	return inst
}

// TestTabbedWindowSetRectClampsNegativeDimensions verifies that SetRect never
// propagates negative content dimensions down to the tab pane. Without
// clamping, tiny terminal windows produce negative ints that later overflow
// to huge uint16 values inside pty.Setsize, corrupting the tmux PTY size. See
// issue #276.
func TestTabbedWindowSetRectClampsNegativeDimensions(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
	}{
		{"zero size", 0, 0},
		{"tiny height", 10, 1},
		{"height just below threshold", 10, 5},
		{"negative inputs", -10, -10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestTabbedWindow()
			setWindowSize(w, tc.width, tc.height)

			previewW, previewH := w.GetPreviewSize()
			assert.GreaterOrEqual(t, previewW, 0, "preview width should be clamped to >= 0")
			assert.GreaterOrEqual(t, previewH, 0, "preview height should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.tab.width, 0, "tab width should be clamped to >= 0")
			assert.GreaterOrEqual(t, w.tab.height, 0, "tab height should be clamped to >= 0")
		})
	}
}

// TestTabbedWindowSetRectNormal sanity-checks that reasonable sizes still
// produce positive content dimensions.
func TestTabbedWindowSetRectNormal(t *testing.T) {
	w := newTestTabbedWindow()
	setWindowSize(w, 200, 100)

	previewW, previewH := w.GetPreviewSize()
	assert.Greater(t, previewW, 0)
	assert.Greater(t, previewH, 0)
}

func TestTabbedWindowResetPreviewScrollUsesCommittedBinding(t *testing.T) {
	alpha := startedWindowInstance(t, "alpha")
	beta := startedWindowInstance(t, "beta")
	w := newTestTabbedWindow()
	setWindowInstance(w, alpha)
	setWindowSize(w, 100, 30)
	require.True(t, w.JumpToTab(1), "precondition: original pane is alpha's terminal tab")

	w.SetPreview(beta, 0, "alpha · › Terminal")
	w.InvalidateContent(beta, 0, "Loading preview…")
	require.NoError(t, w.UpdateContentAt(beta, 0, w.ContentSeq()),
		"the detached snapshot establishes the preview target's host owner")
	w.ScrollUp()
	require.True(t, w.IsInScrollMode(), "precondition: preview target is scrolled")

	require.NoError(t, w.ResetToNormalMode(alpha))
	assert.False(t, w.IsInScrollMode())
	assert.Same(t, alpha, w.tab.currentInstance)
	assert.Equal(t, 1, w.tab.currentTab,
		"reset must use the committed tab, not the preview target's agent tab")
}

// TestTabbedWindowViewIsExactlyRectSized enforces the layout.Pane contract
// (#1024 PR 4): View() is exactly Rect-sized — every line exactly rect.W
// printable cells, exactly rect.H lines — so the root model can tile the
// regions with no clipping math.
func TestTabbedWindowViewIsExactlyRectSized(t *testing.T) {
	w := newTestTabbedWindow()
	setWindowSize(w, 100, 30)
	w.tab.content = tabContentState{text: "content"}

	rendered := w.View()
	lines := strings.Split(rendered, "\n")
	assert.Len(t, lines, 30, "View must render exactly rect.H lines")
	for i, line := range lines {
		assert.Equalf(t, 100, lipgloss.Width(line), "line %d must be exactly rect.W cells", i)
	}
}

// TestTabbedWindowHeaderEllipsizesAtNarrowWidth pins #1098 finding 2: at a
// 40x10 terminal the pane header used to hard-cut (`alpha · Termina`); the
// cut must be marked with an ellipsis instead.
func TestTabbedWindowHeaderEllipsizesAtNarrowWidth(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedRemoteInstance(t)
	w := newTestTabbedWindow()
	setWindowInstance(w, inst)

	// " remote-tabbar · Agent " needs 25 cells; give it 16.
	header := w.renderHeader(16)
	assert.LessOrEqual(t, lipgloss.Width(header), 16, "header must fit the pane width")
	assert.Contains(t, header, "…", "the cut must be marked with an ellipsis")
	assert.NotContains(t, header, "Agent", "the tail is truncated")
}

func TestTabbedWindowScrollCueIsPaneChrome(t *testing.T) {
	inst := startedWindowInstance(t, "scroll-cue")
	w := newTestTabbedWindow()
	setWindowInstance(w, inst)
	setWindowSize(w, 100, 30)
	w.SetScrollOwner(ScrollOwnerHostHistory)

	require.NotContains(t, w.renderHeader(98), "Scroll")
	w.ScrollUp()
	require.True(t, w.IsInScrollMode())
	header := w.renderHeader(98)
	require.Contains(t, header, "Scroll")
	require.Contains(t, header, "Esc exits")
}

// TestTabbedWindowHeaderKeyboardCueStaysInsideBoundIdentity pins the #2579
// clause separator convention once interactive mode coexists with a divergent
// tree selection. The `· keyboard` cue is an identity fragment (joined with
// ` · `), so it must land INSIDE the bound identity — before the `— selected:`
// clause — not at the end of the header where the append used to drop it inside
// the selected session's clause.
func TestTabbedWindowHeaderKeyboardCueStaysInsideBoundIdentity(t *testing.T) {
	alpha := startedWindowInstance(t, "alpha")
	beta := startedWindowInstance(t, "beta")
	const width = 120

	t.Run("selection clause still set off with a dash when not interactive", func(t *testing.T) {
		w := newTestTabbedWindow()
		setWindowInstance(w, alpha)
		w.SetSelectionHint("beta · Agent")
		header := w.renderHeader(width)
		assert.Contains(t, header, "alpha · Agent — selected: beta · Agent")
		assert.NotContains(t, header, "· selected:",
			"the selection clause must not read as another identity fragment")
		assert.NotContains(t, header, "keyboard")
	})

	t.Run("keyboard cue is a fragment of the bound identity without selection", func(t *testing.T) {
		w := newTestTabbedWindow()
		setWindowInstance(w, alpha)
		w.SetInteractive(true)
		header := w.renderHeader(width)
		assert.Contains(t, header, "alpha · Agent · keyboard")
		assert.NotContains(t, header, "— selected:")
	})

	t.Run("keyboard cue precedes the selection clause, not inside it", func(t *testing.T) {
		w := newTestTabbedWindow()
		setWindowInstance(w, alpha)
		w.SetSelectionHint("beta · Agent")
		w.SetInteractive(true)
		header := w.renderHeader(width)
		assert.Contains(t, header, "alpha · Agent · keyboard — selected: beta · Agent",
			"the keyboard cue is a bound-identity fragment and must precede the selected clause")
		assert.NotContains(t, header, "alpha · Agent — selected: beta · Agent · keyboard",
			"the cue must not drop inside the selected session's clause")
	})

	t.Run("scroll clause follows the selection clause which follows the keyboard cue", func(t *testing.T) {
		w := newTestTabbedWindow()
		setWindowInstance(w, alpha)
		w.SetSelectionHint("beta · Agent")
		w.SetInteractive(true)
		w.ScrollUp()
		require.True(t, w.IsInScrollMode())
		header := w.renderHeader(width)
		assert.Contains(t, header, "alpha · Agent · keyboard — selected: beta · Agent — Scroll · Esc exits",
			"identity fragments join with '·', clauses set off with '—' in order: bound identity, selection, scroll")
	})

	t.Run("preview arm keeps keyboard as an identity fragment and ignores the selection hint", func(t *testing.T) {
		w := newTestTabbedWindow()
		setWindowInstance(w, alpha)
		w.SetPreview(beta, 0, "alpha · Agent")
		w.SetSelectionHint("beta · Agent")
		w.SetInteractive(true)
		header := w.renderHeader(width)
		assert.Contains(t, header, "beta · Agent · preview · keyboard",
			"the preview arm joins both fragments inside its identity")
		assert.NotContains(t, header, "— selected:",
			"a previewing pane shows the selection as the preview, so no selected clause is appended")
	})
}

package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// These tests guard the search-overlay close path in handleStateSearch
// (app/handle_overlay.go). Closing the search after a SUBMIT changes the
// sidebar selection via m.sidebar.SelectInstance, and every other
// selection-changing entry point in the TUI then re-homes the focus ring onto
// the tree (handleTaskTrigger, tree nav via focusTreeForNav, mouse wheel and
// row clicks, project switch/delete, Esc out of the Automations/Projects
// rails). The search close predates the focus ring (it originated in 4a20576b
// before the a1f092ad layout cutover introduced the ring), so it never gained
// the re-home and became the only selection-changing entry point that leaves
// the ring stale.
//
// The user-visible failure: after a search submit the screen shows the
// searched session, but the ring stays wherever `/` was pressed — on a
// workspace pane, so `o` attaches the pane's stale COMMITTED binding instead
// of the searched session (the wrong-target class closed repeatedly elsewhere,
// e.g. #1233); or on the Projects rail, where `o` is an inert no-op and the
// searched session is unreachable until the user manually refocuses the tree.
//
// The fix mirrors handleTaskTrigger: call m.focusRegion(layout.RegionTree)
// after SelectInstance in the submitted close branch. The mouse submit path
// closes through this same handler, so it is covered by the same fix.

// typeSearchQuery enters a query into the live search overlay one rune at a
// time, mirroring individual keystrokes (and the convention used by the
// existing #1008 search-overlay test). A query whose runes match exactly one
// instance leaves the overlay with that instance as the sole result, so an
// Enter submit selects it.
func typeSearchQuery(t *testing.T, h *home, query string) {
	t.Helper()
	require.NotNil(t, h.searchOverlay, "search overlay must be open to type a query")
	for _, r := range query {
		h.searchOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// submitSearch presses Enter through handleStateSearch — the real
// state==stateSearch dispatch target — then drives the post-close relayout by
// the host-safe equivalent of the close path's tea.WindowSize() (a direct
// WindowSizeMsg). The layout cutover invariant is that relayout preserves the
// focus ring's active region, so driving it here both faithfully reproduces
// the post-close UI state and proves the re-home survives the resize rather
// than being undone by it.
func submitSearch(t *testing.T, h *home) {
	t.Helper()
	require.Equal(t, stateSearch, h.state, "search overlay must be open for submit")
	h.handleStateSearch(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateDefault, h.state, "submit must close the search overlay back to default")
	require.Nil(t, h.searchOverlay, "submit must drop the search overlay")
	// tea.WindowSize() queries the live terminal, which a hermetic test has
	// none of; the direct WindowSizeMsg path is what it ultimately drives, and
	// is what the close path's resize is for. selectionChanged()'s
	// synchronous body (which installs panePreviewTxn) already ran during
	// handleStateSearch's return construction; the async capture half is not
	// needed for these assertions, so the returned cmd is left undriven.
	resizeHome(h, 200, 40)
}

// driveAttachKey presses `o` (the attach verb) through the full Update
// dispatch — the path the real key takes, including the Projects/Automations
// captive handlers and pane-focus contextual handlers — and drives the
// deferred attach transition to completion so the attachOverlayCallbackFn seam
// fires. Returns the title of the session the attach actually targeted, or ""
// when no attach was initiated (e.g. the key was consumed as an inert no-op
// before reaching handleAttach). The first-time attach help overlay is marked
// seen so the attach callback runs synchronously inside the transition rather
// than being deferred behind the help screen.
func driveAttachKey(t *testing.T, h *home) string {
	t.Helper()
	var attachedTitle string
	swapAttachOverlayCallbackFn(t, func(_ *home, target sessionActionTarget, _, _ string, _ func() (chan struct{}, error)) tea.Cmd {
		attachedTitle = target.title
		return nil
	})
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()),
		"first-time attach help must be seen so the attach callback runs synchronously")
	mod, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	h = mod.(*home)
	runAttachTransitionCmd(t, h, cmd)
	return attachedTitle
}

// TestSearchClose_SubmitRehomesRingToTree_FromPaneFocus is the wrong-target
// regression: with the ring on a workspace pane bound to a DIFFERENT session,
// a search submit must re-home the ring to the tree so the following `o`
// attaches the session the user just searched for, not the pane's stale
// committed binding.
//
// Before the fix the ring stayed on the pane, selectionChanged installed a
// live preview (pane bound to alpha, previewing beta) that the screen showed,
// but handleAttach routed through handleEnterPane and read p.Instance() — the
// COMMITTED binding alpha — so `o` attached alpha while beta was on screen,
// with no error or notice.
func TestSearchClose_SubmitRehomesRingToTree_FromPaneFocus(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// `s` with tree focus opens the selection (alpha, tab 0) as a focused pane.
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes(), "s must open the selection as a pane")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: the pane is committed to alpha")
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"precondition: the focus ring rests on the pane, not the tree")

	// Open the search overlay exactly as the `/` key does; the overlay does
	// not move the ring, so it stays on the pane.
	_, _ = h.showSearchOverlay()
	require.Equal(t, stateSearch, h.state)
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"the search overlay must not move the ring while it is open")

	// Search for beta and submit. The query matches only beta, so the overlay
	// selects it and the close branch re-homes the ring to the tree.
	typeSearchQuery(t, h, "beta")
	submitSearch(t, h)

	// `o` now attaches the searched session, not the pane's stale binding.
	// (Probe order: this user-visible symptom is asserted before the ring
	// invariant so a RED run with the fix reverted surfaces the wrong-target
	// attach — the bug's headline — rather than stopping at the ring check.)
	attachedTitle := driveAttachKey(t, h)
	require.Equal(t, "beta", attachedTitle,
		"`o` after a search submit must attach the searched session, not the focused pane's committed binding")

	// The ring is now on the tree — the invariant every other selection path
	// already applied. It survives the post-close relayout (driven inside
	// submitSearch), proving the re-home is not undone by tea.WindowSize().
	require.Equal(t, layout.RegionTree, h.ring.Active(),
		"submitted search close must re-home the focus ring to the tree")

	// The preview mechanism the bug depended on is still active and still
	// shows beta on the alpha pane, while the pane's COMMITTED binding stays
	// alpha. This is exactly the divergent state that made `o` attach the
	// wrong session before the fix: the screen shows beta, p.Instance() is
	// alpha. With the ring now on the tree, `o` reads the tree selection
	// (beta) instead of the pane's committed binding, so the divergence no
	// longer matters for the attach verb.
	require.NotNil(t, h.panePreviewTxn, "the preview of the searched session is live on the focused pane")
	require.Equal(t, alphaPane.ID(), h.panePreviewTxn.ownerPaneID, "the preview is owned by the alpha pane")
	require.Same(t, beta, h.panePreviewTxn.target.instance, "the preview targets the searched session beta")
	require.Same(t, alpha, alphaPane.Instance(), "the pane's committed binding stays alpha (preview is transient)")
}

// TestSearchClose_SubmitRehomesRingToTree_FromProjectsFocus is the
// unreachable-session regression: with the ring on the Projects rail (where `/`
// is the only key that enters search, routed explicitly by handleProjectsFocus),
// a search submit must re-home the ring to the tree so `o` can attach the
// searched session at all. Before the fix the ring stayed on RegionProjects,
// where handleProjectsFocus consumes every non-chrome key — including `o` — as
// an inert no-op, so the attach seam was never reached and nothing happened.
func TestSearchClose_SubmitRehomesRingToTree_FromProjectsFocus(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")

	// The Projects rail is a Tab-focusable section once it has rows; the focus
	// ring can land on it. `/` is the ONLY key that enters search from there.
	require.True(t, h.projects.HasProjects(), "precondition: the Projects rail has rows")
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active(),
		"precondition: the focus ring rests on the Projects rail")

	// Open search and submit a query for beta. The overlay does not move the
	// ring, so before the fix it would stay on RegionProjects through the close.
	_, _ = h.showSearchOverlay()
	typeSearchQuery(t, h, "beta")
	submitSearch(t, h)

	// `o` reaches handleAttach (not swallowed by the Projects no-op) and
	// attaches the searched session. Before the fix the seam was never
	// invoked, so attachedTitle stayed "".
	// (Probe order: this user-visible symptom is asserted before the ring
	// invariant so a RED run with the fix reverted surfaces the inert no-op —
	// the bug's headline — rather than stopping at the ring check.)
	attachedTitle := driveAttachKey(t, h)
	require.Equal(t, "beta", attachedTitle,
		"`o` after a search submit from Projects focus must attach the searched session, not be swallowed as an inert no-op")

	// The ring is re-homed to the tree, releasing the Projects rail's captive
	// hold so selection-scoped verbs can act on the searched session.
	require.Equal(t, layout.RegionTree, h.ring.Active(),
		"submitted search close must re-home the focus ring off the Projects rail to the tree")

	// Pin the selection state too: the sidebar now points at beta, which is
	// what the tree-path attach read above.
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"the searched session must be the sidebar selection after the submit")
}

// TestSearchClose_CancelPreservesFocusRing locks in the SCOPE of the fix: the
// re-home applies to a SUBMIT, which is the path that changes the selection. A
// CANCEL (Esc, no submit) leaves the selection unchanged, so the ring must stay
// wherever the user was when they opened the search — the same "return to what
// I was doing" contract a modal cancel has everywhere else in the TUI. This
// guards against an over-broad re-home that would yank focus to the tree on
// every Esc and disrupt a user who opened search from a pane they intended to
// keep working in.
func TestSearchClose_CancelPreservesFocusRing(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	// Open a pane on alpha; the ring rests on it. The user opens search from
	// here and then cancels.
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"precondition: the focus ring rests on the pane before the search opens")

	_, _ = h.showSearchOverlay()
	require.Equal(t, stateSearch, h.state)

	// Esc cancels: shouldClose with IsSubmitted()==false, so the close branch
	// skips SelectInstance and the re-home — the canonical cancel path
	// (handleStateSearch drives the overlay's HandleKeyPress itself).
	h.handleStateSearch(tea.KeyMsg{Type: tea.KeyEsc})

	require.Equal(t, stateDefault, h.state, "cancel must return to the default state")
	require.Nil(t, h.searchOverlay, "cancel must drop the search overlay")
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"a cancelled search must NOT re-home the ring — the user returns to the pane they opened it from")
	require.Same(t, alpha, h.sidebar.GetSelectedInstance(),
		"a cancelled search must not change the sidebar selection")
}

// TestSearchClose_MouseSubmitRehomesRingToTree covers the mouse submit path
// (G10). A click on a search row sets the overlay's selected index and then
// submits through the SAME handleStateSearch(Enter) handler the keyboard path
// uses (app/handle_mouse.go:726-728), so the focus re-home fix in that shared
// handler covers both paths. This test drives the submit exactly the way the
// mouse handler does — SetSelectedIndex to the target row, then
// handleStateSearch(Enter) — instead of typing a query and arrowing to it.
// With the fix reverted the ring stays on the pane (same bug as the keyboard
// submit, since the close branch is shared).
func TestSearchClose_MouseSubmitRehomesRingToTree(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")

	// `s` opens alpha as a focused pane; the ring rests on it.
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"precondition: the focus ring rests on the pane before the search opens")

	// Open search. The overlay lists every instance in sidebar order, so
	// beta is at index 1.
	_, _ = h.showSearchOverlay()
	require.Equal(t, stateSearch, h.state)
	require.GreaterOrEqual(t, len(h.searchOverlay.ResultInstances()), 2,
		"precondition: the overlay lists at least alpha and beta")

	// Drive the mouse submit exactly as handle_mouse.go does: set the index
	// to the clicked row, then call handleStateSearch(Enter). This is the
	// shared close branch the fix lives in.
	betaIdx := -1
	for i, inst := range h.searchOverlay.ResultInstances() {
		if inst.Title == "beta" {
			betaIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, betaIdx, 0, "precondition: beta must be in the overlay results")
	h.searchOverlay.SetSelectedIndex(betaIdx)
	h.handleStateSearch(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateDefault, h.state, "mouse submit must close the search overlay")
	require.Equal(t, layout.RegionTree, h.ring.Active(),
		"mouse submit must re-home the focus ring to the tree (same close branch as keyboard)")
	require.Same(t, beta, h.sidebar.GetSelectedInstance(),
		"mouse submit must select the clicked session")
}

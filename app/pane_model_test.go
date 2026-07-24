package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// ----------------------------------------------------------------------------
// N-pane model (#1088): open/focus/hide panes, the dynamic focus ring, the
// §2.6 pane-count fitting (auto-hide on shrink, restore on grow), capture
// throttling and the attached pause across N panes, attach of the focused
// pane, and the pane bindings following instance removal / same-title swaps.
// ----------------------------------------------------------------------------

// paneTestHome is a home with three started instances at a three-pane-capable
// size, with the selection on "alpha". Each instance carries a real agent +
// shell tab pair so tab-row walks and second-slot pane opens have two real
// slots (#1100: fresh instances hold only the agent tab and no slot is padded).
func paneTestHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	for _, title := range []string{"alpha", "beta", "gamma"} {
		inst := instanceWithFakeBackend(t, title)
		inst.AddTabForTest("agent", session.TabKindAgent)
		inst.AddTabForTest("shell", session.TabKindShell)
		h.store.AddInstance(inst)
	}
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)
	return h
}

// pressKey drives handleDefaultKeyPress with a raw key string, the full
// dispatch path (menu highlighting re-emit excluded — tests call the handler
// directly like the other model-level suites).
func pressKey(t *testing.T, h *home, key string) {
	t.Helper()
	name, ok := keys.GlobalKeyStringsMap[key]
	require.True(t, ok, "key %q must be mapped", key)
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}, name)
}

// pressTab cycles the focus ring.
func pressTab(t *testing.T, h *home, back bool) {
	t.Helper()
	if back {
		_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyShiftTab}, keys.KeyShiftTab)
		return
	}
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyTab}, keys.KeyTab)
}

// visibleTitles flattens the visible panes to "<title>:<tab>" for assertions.
func visibleTitles(h *home) []string {
	out := make([]string, 0, len(h.visiblePanes))
	for _, p := range h.visiblePanes {
		title := ""
		if p.Instance() != nil {
			title = p.Instance().Title
		}
		out = append(out, title)
	}
	return out
}

type previewTextBackend struct {
	*session.FakeBackend
	text string
}

func (b *previewTextBackend) Preview(*session.Instance) (string, error) {
	return b.text, nil
}

func (b *previewTextBackend) PreviewFullHistory(*session.Instance) (string, error) {
	return b.text, nil
}

func setPreviewText(inst *session.Instance, text string) {
	inst.SetBackend(&previewTextBackend{FakeBackend: session.NewFakeBackend(), text: text})
}

func TestFirstRunWorkspaceEmptyState(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 120, 30)

	view := h.View()

	assert.Contains(t, view, "No sessions yet")
	assert.Contains(t, view, "Press n to create a local session")
	assert.Contains(t, view, "Press ? for all keys")
	assert.Contains(t, view, "af doctor --setup")
	assert.NotContains(t, view, "s opens the selected tab")
}

// TestPane_OpenHideFlow walks the core verb set: s with tree focus opens the
// selection as a focused pane; a second s on another instance opens a second
// pane to the RIGHT; x hides the focused pane, the survivor re-divides the
// full workspace width, and nothing is killed.
func TestPane_OpenHideFlow(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// s with tree focus: open (alpha, tab 0) as a focused pane.
	require.Equal(t, layout.RegionTree, h.ring.Active())
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes(), "s opens the selection as a pane")
	p1 := h.store.OpenPanes()[0]
	assert.Same(t, alpha, p1.Instance())
	assert.Equal(t, 0, p1.Tab())
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "the opened pane takes focus")
	fullWidth := h.lastLayout.Panes[0].W

	// The tree keeps driving the selection without touching the pane.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())
	assert.Same(t, alpha, p1.Instance(), "open panes are explicit bindings, not selection-driven")

	// s again: beta opens as a NEW pane to the right of alpha's.
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())
	assert.Equal(t, []string{"alpha", "beta"}, visibleTitles(h), "new panes open to the right")
	p2 := h.store.OpenPanes()[1]
	assert.Equal(t, layout.PaneRegion(p2.ID()), h.ring.Active())
	assert.Equal(t, 2, h.lastLayout.PaneCount(), "two panes are laid out side by side")
	assert.Less(t, h.lastLayout.Panes[0].W, fullWidth, "panes divide the width")

	// x hides the focused (beta) pane: alpha's pane re-absorbs the full
	// width, focus lands on it, and beta keeps running.
	tabsBefore := len(beta.GetTabs())
	pressKey(t, h, "x")
	require.Equal(t, 1, h.store.NumOpenPanes(), "x hides the focused pane")
	assert.Equal(t, []string{"alpha"}, visibleTitles(h))
	assert.Equal(t, fullWidth, h.lastLayout.Panes[0].W, "the survivor re-divides the full width")
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "focus lands on the surviving pane")
	assert.Equal(t, tabsBefore, len(beta.GetTabs()), "hiding kills nothing")
	assert.True(t, beta.TmuxAlive(), "the hidden tab keeps running in tmux")

	// x on the last pane empties the workspace; focus returns to the tree.
	pressKey(t, h, "x")
	require.Zero(t, h.store.NumOpenPanes())
	assert.Equal(t, layout.RegionTree, h.ring.Active())
	assert.Contains(t, h.View(), "no panes open", "the empty workspace advertises the open verb")
}

// TestPane_OpenAlreadyOpenFocuses: s on a tab that is already open as a pane
// focuses that pane instead of duplicating it (§2.3).
func TestPane_OpenAlreadyOpenFocuses(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	p1 := h.store.OpenPanes()[0]

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Back to alpha (still open in pane 1): s focuses, no third pane.
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	assert.Equal(t, 2, h.store.NumOpenPanes(), "an already-open tab must not open twice")
	assert.Equal(t, layout.PaneRegion(p1.ID()), h.ring.Active(), "s focuses the existing pane")
}

// TestPane_HeaderAnnotatesSelectionDivergence is the #1289 session-level
// reconciliation guard with #1321 previews layered on: preview state must name
// both the transient target and original pane, and canceling the preview must
// restore the old selected-vs-shown header invariant.
func TestPane_HeaderAnnotatesSelectionDivergence(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Same(t, beta, h.store.GetSelectedInstance())
	require.Same(t, alpha, paneA.Instance(), "selection must not retarget explicit panes")

	view := h.View()
	assert.Contains(t, view, "Preview beta · ◆ Agent (original alpha · ◆ Agent)",
		"preview header must reconcile transient target vs original pane")
	assert.NotContains(t, view, "alpha · ◆ Agent · selected: beta · ◆ Agent",
		"selected divergence is hidden while preview owns the render binding")

	h.cancelPanePreview(false)
	view = h.View()
	assert.Contains(t, view, "alpha · ◆ Agent · selected: beta · ◆ Agent",
		"canceling preview restores the #1289 selected row vs shown content invariant")
}

func TestPanePreviewPaintsLastCaptureWhileRefreshing(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	setPreviewText(alpha, "ALPHA_PREVIEW_CONTENT")
	setPreviewText(beta, "BETA_PREVIEW_CONTENT")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	w := h.paneWindows[paneA.ID()]
	require.NotNil(t, w)
	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, alpha, 0, w.ContentSeq())())
	require.Contains(t, h.View(), "ALPHA_PREVIEW_CONTENT")

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()

	view := h.View()
	assert.Contains(t, view, "Preview beta · ◆ Agent (original alpha · ◆ Agent)")
	assert.Contains(t, view, "ALPHA_PREVIEW_CONTENT",
		"retargeting must paint the last capture instead of blanking while beta loads")
	assert.NotContains(t, view, "Loading preview…")
	assert.Same(t, alpha, paneA.Instance(), "preview must not mutate the committed pane binding")
	assert.Same(t, beta, h.panePreviewTxn.target.instance)

	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, beta, 0, h.panePreviewTxn.seq)())
	view = h.View()
	assert.Contains(t, view, "BETA_PREVIEW_CONTENT",
		"the daemon capture must replace stale content in place when it lands")
	assert.NotContains(t, view, "ALPHA_PREVIEW_CONTENT")
}

func TestPanePreviewSlowCaptureFallsBackAfterGrace(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	setPreviewText(alpha, "ALPHA_PREVIEW_CONTENT")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	w := h.paneWindows[paneA.ID()]
	require.NotNil(t, w)
	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, alpha, 0, w.ContentSeq())())

	started := time.Now()
	graceCmd := h.updatePanePreview(beta, 0, false, false)
	require.NotNil(t, graceCmd, "a new preview target must arm the stale-frame grace period")
	assert.Contains(t, h.View(), "ALPHA_PREVIEW_CONTENT",
		"the completed frame stays visible during the fast-path grace period")
	assert.Nil(t, h.updatePanePreview(beta, 0, false, false),
		"refresh ticks for the same target must not keep restarting the grace period")

	msg := graceCmd()
	assert.GreaterOrEqual(t, time.Since(started), panePreviewStaleGrace)
	require.IsType(t, panePreviewStaleExpiredMsg{}, msg)
	_, followup := h.Update(msg)
	assert.Nil(t, followup)

	view := h.View()
	assert.Contains(t, view, "Preview beta · ◆ Agent (original alpha · ◆ Agent)")
	assert.Contains(t, view, "Loading preview…",
		"a capture that does not arrive must stop showing another session's pane")
	assert.NotContains(t, view, "ALPHA_PREVIEW_CONTENT")
}

func TestPanePreviewCompletedCaptureWinsGraceExpiry(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	setPreviewText(alpha, "ALPHA_PREVIEW_CONTENT")
	setPreviewText(beta, "BETA_PREVIEW_CONTENT")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	w := h.paneWindows[paneA.ID()]
	require.NotNil(t, w)
	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, alpha, 0, w.ContentSeq())())

	require.NotNil(t, h.updatePanePreview(beta, 0, false, false))
	txn := *h.panePreviewTxn
	expiry := panePreviewStaleExpiredMsg{
		ownerPaneID:    txn.ownerPaneID,
		target:         txn.target,
		seq:            txn.seq,
		renderRevision: w.RenderRevision(),
	}
	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, beta, 0, txn.seq)())

	_, followup := h.Update(expiry)
	assert.Nil(t, followup)
	view := h.View()
	assert.Contains(t, view, "BETA_PREVIEW_CONTENT",
		"the grace timer must not overwrite a capture that already landed")
	assert.NotContains(t, view, "Loading preview…")
}

func TestPanePreviewRetargetHonorsCaptureThrottle(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	recent := time.Now()
	h.lastPaneCapture[paneA.ID()] = recent

	h.updatePanePreview(beta, 0, false, false)
	assert.Equal(t, recent, h.lastPaneCapture[paneA.ID()],
		"arrowing to a new preview must not bypass the per-pane capture cadence")
	assert.Nil(t, h.panesRefresh(false),
		"a selection inside the 100ms window must reuse the next scheduled refresh")
}

func TestPanePreviewSplitHintRequiresActivePreview(t *testing.T) {
	h := paneTestHome(t)

	pressKey(t, h, "s")
	require.Nil(t, h.panePreviewTxn)
	assert.NotContains(t, h.menu.String(), "split pane",
		"the footer must not advertise S when there is no preview to commit")

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	assert.Contains(t, h.menu.String(), "split pane",
		"the footer should advertise S while a preview can be committed")

	h.cancelPanePreview(false)
	assert.NotContains(t, h.menu.String(), "split pane",
		"canceling the preview must remove the split hint immediately")
}

func TestPanePreviewFastScrollLatestWins(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	gamma := h.store.GetInstanceByTitle("gamma")
	setPreviewText(alpha, "ALPHA_PREVIEW_CONTENT")
	setPreviewText(beta, "BETA_PREVIEW_CONTENT")
	setPreviewText(gamma, "GAMMA_PREVIEW_CONTENT")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	w := h.paneWindows[paneA.ID()]
	require.NotNil(t, w)
	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, alpha, 0, w.ContentSeq())())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	betaSeq := h.panePreviewTxn.seq
	betaExpiry := panePreviewStaleExpiredMsg{
		ownerPaneID:    h.panePreviewTxn.ownerPaneID,
		target:         h.panePreviewTxn.target,
		seq:            betaSeq,
		renderRevision: w.RenderRevision(),
	}

	h.sidebar.SetSelectedInstance(2)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	gammaSeq := h.panePreviewTxn.seq
	require.NotEqual(t, betaSeq, gammaSeq)
	_, _ = h.Update(betaExpiry)
	assert.NotContains(t, h.View(), "Loading preview…",
		"the grace timer for an older target must not blank the newest preview")

	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, beta, 0, betaSeq)())
	view := h.View()
	assert.Contains(t, view, "Preview gamma · ◆ Agent (original alpha · ◆ Agent)")
	assert.Contains(t, view, "ALPHA_PREVIEW_CONTENT",
		"a late capture for beta must leave the last painted frame in place")
	assert.NotContains(t, view, "BETA_PREVIEW_CONTENT",
		"late beta capture must not overwrite the newer gamma preview target")

	require.IsType(t, panesRefreshedMsg{}, refreshPaneBindingCmd(w, gamma, 0, gammaSeq)())
	view = h.View()
	assert.Contains(t, view, "Preview gamma · ◆ Agent (original alpha · ◆ Agent)")
	assert.Contains(t, view, "GAMMA_PREVIEW_CONTENT")
	assert.NotContains(t, view, "BETA_PREVIEW_CONTENT")
	assert.Same(t, alpha, paneA.Instance(), "latest-wins preview must still be transient")
}

func TestPanePreviewEscFromScrollRevertsOriginalCommittedTab(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	setPreviewText(beta, "BETA_PREVIEW_HISTORY")

	_, _ = h.openOrFocusPane(alpha, 1)
	paneA := h.store.OpenPanes()[0]
	w := h.paneWindows[paneA.ID()]
	require.NotNil(t, w)
	require.Equal(t, 1, paneA.Tab(), "precondition: original pane is alpha's terminal tab")

	h.store.SetActiveTab(0)
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	require.True(t, w.Previewing())
	require.IsType(t, panesRefreshedMsg{},
		refreshPaneBindingCmd(w, beta, 0, h.panePreviewTxn.seq)())

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyCtrlU}, keys.KeyShiftUp)
	require.True(t, w.IsInScrollMode(), "precondition: preview pane is in scroll mode")

	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.NotNil(t, cmd, "Esc should schedule an original-pane refresh after canceling preview")

	require.Nil(t, h.panePreviewTxn, "Esc from preview scroll must cancel the transient preview")
	require.False(t, w.Previewing())
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active(), "Esc cancel focuses the owner pane")
	assert.Same(t, alpha, paneA.Instance())
	assert.Equal(t, 1, paneA.Tab(), "Esc must restore alpha's original tab, not alpha's agent tab")

	view := h.View()
	assert.Contains(t, view, "alpha · › Terminal")
	assert.NotContains(t, view, "Preview beta")
	assert.NotContains(t, view, "alpha · ◆ Agent",
		"reset must not pair the committed alpha instance with the preview agent tab")
}

func TestPanePreviewTabRowCommitsSameInstanceTerminal(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())
	require.Equal(t, 0, paneA.Tab(), "precondition: alpha's agent pane is open")

	// Walk the tree cursor onto alpha's terminal tab row. The selected tab is
	// a full (instance, tab) target, not "any open pane for this instance".
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.NotNil(t, h.panePreviewTxn)
	assert.Same(t, alpha, h.panePreviewTxn.target.instance)
	assert.Equal(t, 1, h.panePreviewTxn.target.tab)
	assert.Equal(t, 0, paneA.Tab(), "preview remains transient until commit")
	assert.Contains(t, h.View(), "Preview alpha · › Terminal (original alpha · ◆ Agent)")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

	require.Nil(t, h.panePreviewTxn)
	assert.Equal(t, 1, h.store.NumOpenPanes(), "commit-replace must not duplicate panes")
	assert.Same(t, paneA, h.store.FindOpenPane(alpha, 1))
	assert.Nil(t, h.store.FindOpenPane(alpha, 0),
		"the already-open agent pane must not be treated as the terminal target")
	assert.Equal(t, 1, paneA.Tab(), "the focused pane binds the selected terminal tab")
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active())
}

func TestPanePreviewSelectionFocusesAlreadyOpenTabPane(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	paneAgent := openTestPane(t, h, alpha, 0)
	paneTerminal := openTestPane(t, h, alpha, 1)
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.focusRegion(layout.PaneRegion(paneAgent.ID()))
	h.focusRegion(layout.RegionTree)

	pressNav(t, h, "j") // Agent tab row: same as the preview owner.
	require.Nil(t, h.panePreviewTxn)
	require.Equal(t, layout.RegionTree, h.ring.Active(),
		"selecting the owner binding must preserve tree focus")

	pressNav(t, h, "j") // Terminal tab row: already open in paneTerminal.

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsTab)
	require.Equal(t, 1, sel.TabIndex)
	require.Nil(t, h.panePreviewTxn, "already-open tab rows must not create previews")
	assert.Equal(t, layout.PaneRegion(paneTerminal.ID()), h.ring.Active(),
		"selection must jump to the existing tab pane")
	assert.Same(t, alpha, paneAgent.Instance())
	assert.Equal(t, 0, paneAgent.Tab())
	assert.Same(t, alpha, paneTerminal.Instance())
	assert.Equal(t, 1, paneTerminal.Tab())
	assert.NotContains(t, h.View(), "Preview")
}

func TestPanePreviewInstanceRowUsesSelectedTerminalTab(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())
	require.Equal(t, 0, paneA.Tab(), "precondition: alpha's agent pane is open")

	h.sidebar.SetSelectedInstance(1)
	h.store.SetActiveTab(1)
	_ = h.selectionChanged()

	sel := h.sidebar.GetSelection()
	require.False(t, sel.IsTab, "precondition: cursor remains on beta's instance row")
	require.Equal(t, 1, h.store.ActiveTab(), "precondition: selected/action tab is Terminal")
	require.NotNil(t, h.panePreviewTxn)
	assert.Same(t, beta, h.panePreviewTxn.target.instance)
	assert.Equal(t, 1, h.panePreviewTxn.target.tab,
		"preview target must match the selected/action (instance, tab), not default to Agent")
	assert.Contains(t, h.View(), "Preview beta · › Terminal (original alpha · ◆ Agent)")
	assert.NotContains(t, h.View(), "Preview beta · ◆ Agent")
}

func TestPanePreviewEnterCommitsReplace(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

	require.NotNil(t, cmd, "commit-replace should schedule a refresh for the committed pane")
	require.Nil(t, h.panePreviewTxn)
	assert.Same(t, beta, paneA.Instance(), "Enter commits the highlighted preview into the owner pane")
	assert.Equal(t, 0, paneA.Tab())
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active())
	view := h.View()
	assert.Contains(t, view, "beta · ◆ Agent")
	assert.NotContains(t, view, "Preview")
}

func TestPanePreviewEnterCommitFocusesAlreadyOpenTarget(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	paneB := h.openPaneWindow(beta, 0)
	require.NotNil(t, paneB)
	h.relayout()
	require.Equal(t, 2, h.store.NumOpenPanes())
	require.Same(t, beta, paneB.Instance())

	h.focusRegion(layout.PaneRegion(paneA.ID()))
	require.NotNil(t, h.panePreviewTxn)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

	require.NotNil(t, cmd, "focusing the existing target should schedule a refresh")
	require.Nil(t, h.panePreviewTxn)
	assert.Same(t, alpha, paneA.Instance(), "owner pane must keep its original binding")
	assert.Same(t, beta, paneB.Instance())
	assert.Equal(t, 2, h.store.NumOpenPanes(), "commit onto an already-open target must not duplicate panes")
	assert.Same(t, paneB, h.store.FindOpenPane(beta, 0))
	assert.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active(), "existing target pane takes focus")
}

func TestPanePreviewSplitCommitsAlongside(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeySplitPane)

	require.NotNil(t, cmd, "commit-alongside should schedule a refresh")
	require.Nil(t, h.panePreviewTxn)
	require.Equal(t, 2, h.store.NumOpenPanes())
	paneB := h.store.OpenPanes()[1]
	assert.Same(t, alpha, paneA.Instance(), "split must restore the owner pane's original binding")
	assert.Equal(t, 0, paneA.Tab())
	assert.Same(t, beta, paneB.Instance(), "split appends the highlighted preview target")
	assert.Equal(t, 0, paneB.Tab())
	assert.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active(), "new target pane takes focus")
	view := h.View()
	assert.Contains(t, view, "alpha · ◆ Agent")
	assert.Contains(t, view, "beta · ◆ Agent")
	assert.NotContains(t, view, "Preview")
}

func TestPanePreviewSplitHideDoesNotStickInPanePreview(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	// alpha tab 0 -> alpha tab 1 -> beta tab 0 -> beta tab 1.
	for i := 0; i < 4; i++ {
		pressNav(t, h, "j")
	}
	require.Same(t, beta, h.store.GetSelectedInstance())
	require.Equal(t, layout.RegionTree, h.ring.Active(), "tree navigation owns focus during preview")
	require.NotNil(t, h.panePreviewTxn)
	require.Contains(t, h.View(), "Preview beta · › Terminal (original alpha · ◆ Agent)")

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeySplitPane)
	require.NotNil(t, cmd)
	require.Nil(t, h.panePreviewTxn)
	require.Equal(t, 2, h.store.NumOpenPanes())
	paneB := h.store.OpenPanes()[1]
	require.Same(t, beta, paneB.Instance())
	require.Equal(t, 1, paneB.Tab())
	require.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active())

	pressTab(t, h, false)
	require.Equal(t, layout.RegionAutomations, h.ring.Active())
	pressTab(t, h, true)
	require.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active())

	pressKey(t, h, "x")

	require.Equal(t, 1, h.store.NumOpenPanes())
	require.Same(t, paneA, h.store.OpenPanes()[0])
	require.Nil(t, h.panePreviewTxn, "hiding the split target must not recreate its preview")
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active(), "focus lands on the surviving pane")
	view := h.View()
	assert.Contains(t, view, "alpha · ◆ Agent · selected: beta ·",
		"the survivor keeps the #1289 selected-vs-shown header")
	assert.NotContains(t, view, "Preview", "the hidden split pane must not leave a transient preview")

	_ = h.selectionChanged()
	require.Nil(t, h.panePreviewTxn, "the preview tick must not recreate the dismissed split target")

	pressTab(t, h, false)
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(), "Tab must cycle out of pane context")
	pressTab(t, h, false)
	assert.Equal(t, layout.RegionProjects, h.ring.Active(), "Tab continues into the Projects section")
	pressTab(t, h, false)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "the focus ring must remain able to reach the tree")
}

func TestPanePreviewSplitFocusesAlreadyOpenTarget(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)
	paneB := h.openPaneWindow(beta, 0)
	require.NotNil(t, paneB)
	h.relayout()
	require.Equal(t, 2, h.store.NumOpenPanes())
	require.Same(t, beta, paneB.Instance())

	h.focusRegion(layout.PaneRegion(paneA.ID()))
	require.NotNil(t, h.panePreviewTxn)

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeySplitPane)

	require.NotNil(t, cmd, "focusing the existing target should schedule a refresh")
	require.Nil(t, h.panePreviewTxn)
	assert.Same(t, alpha, paneA.Instance(), "owner pane must keep its original binding")
	assert.Same(t, beta, paneB.Instance())
	assert.Equal(t, 2, h.store.NumOpenPanes(), "split onto an already-open target must not duplicate panes")
	assert.Same(t, paneB, h.store.FindOpenPane(beta, 0))
	assert.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active(), "existing target pane takes focus")
}

// TestPanePreviewEscCancelsToOwnerPane: Esc dismisses a live preview and leaves
// focus on the owner pane — "escape the preview, stay put". (Tab, by contrast,
// dismisses AND advances the ring — see TestPanePreviewTabDismissesAndAdvances.)
func TestPanePreviewEscCancelsToOwnerPane(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)

	_, cmd := h.Update(tea.KeyMsg{Type: tea.KeyEsc})

	require.NotNil(t, cmd, "cancel should schedule an owner-pane refresh")
	require.Nil(t, h.panePreviewTxn)
	assert.Same(t, alpha, paneA.Instance())
	assert.Equal(t, 0, paneA.Tab())
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active())
	view := h.View()
	assert.Contains(t, view, "alpha · ◆ Agent · selected: beta · ◆ Agent")
	assert.NotContains(t, view, "Preview")
}

// TestPanePreviewTabDismissesAndAdvances: Tab over a live preview must dismiss
// the preview AND still step the focus ring — never swallow the keystroke to sit
// on the owner pane (the #1705 class, in the forward direction). With paneA the
// only pane and focus on it, forward Tab lands on the next section (Automations).
func TestPanePreviewTabDismissesAndAdvances(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active())
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn)

	pressTab(t, h, false)

	require.Nil(t, h.panePreviewTxn, "Tab dismisses the transient preview")
	assert.Same(t, alpha, paneA.Instance(), "owner pane reverts to its real binding")
	assert.Equal(t, layout.RegionAutomations, h.ring.Active(),
		"Tab advances the ring off the pane rather than swallowing the keystroke")
	assert.NotContains(t, h.View(), "Preview")
}

func TestPanePreviewEnterBlocksUncommittableTargets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*session.Instance)
		wantErr   string
	}{
		{
			name:      "dead",
			configure: func(inst *session.Instance) { _ = inst.Transition(session.ObserveLiveness(session.LiveDead)) },
			wantErr:   "no longer running",
		},
		{
			name:      "lost",
			configure: func(inst *session.Instance) { _ = inst.Transition(session.ObserveLiveness(session.LiveLost)) },
			wantErr:   "was lost",
		},
		{
			name:      "in-flight",
			configure: func(inst *session.Instance) { inst.SetInFlightOpForTest(session.OpKilling) },
			wantErr:   "operation in flight",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := paneTestHome(t)
			alpha := h.store.GetInstanceByTitle("alpha")
			beta := h.store.GetInstanceByTitle("beta")
			tc.configure(beta)

			pressKey(t, h, "s")
			paneA := h.store.OpenPanes()[0]
			h.sidebar.SetSelectedInstance(1)
			_ = h.selectionChanged()
			require.NotNil(t, h.panePreviewTxn, "preview remains allowed for blocked commit targets")

			_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyEnter}, keys.KeyEnter)

			require.NotNil(t, h.panePreviewTxn, "blocked commit should keep the preview active")
			assert.Same(t, alpha, paneA.Instance())
			assert.Contains(t, h.errBox.String(), tc.wantErr)
		})
	}
}

func TestPanePreviewSplitBlocksUncommittableTargets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*session.Instance)
		wantErr   string
	}{
		{
			name:      "dead",
			configure: func(inst *session.Instance) { _ = inst.Transition(session.ObserveLiveness(session.LiveDead)) },
			wantErr:   "no longer running",
		},
		{
			name:      "lost",
			configure: func(inst *session.Instance) { _ = inst.Transition(session.ObserveLiveness(session.LiveLost)) },
			wantErr:   "was lost",
		},
		{
			name:      "in-flight",
			configure: func(inst *session.Instance) { inst.SetInFlightOpForTest(session.OpKilling) },
			wantErr:   "operation in flight",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := paneTestHome(t)
			alpha := h.store.GetInstanceByTitle("alpha")
			beta := h.store.GetInstanceByTitle("beta")
			tc.configure(beta)

			pressKey(t, h, "s")
			paneA := h.store.OpenPanes()[0]
			h.sidebar.SetSelectedInstance(1)
			_ = h.selectionChanged()
			require.NotNil(t, h.panePreviewTxn, "preview remains allowed for blocked commit targets")

			_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")}, keys.KeySplitPane)

			require.NotNil(t, h.panePreviewTxn, "blocked split should keep the preview active")
			assert.Equal(t, 1, h.store.NumOpenPanes(), "blocked split must not append a pane")
			assert.Same(t, alpha, paneA.Instance())
			assert.Contains(t, h.errBox.String(), tc.wantErr)
		})
	}
}

// TestPane_TabDimension: opening from a tree TAB row binds that tab, distinct
// (instance, tab) pairs get distinct panes, and later selection tab jumps
// don't touch open panes.
func TestPane_TabDimension(t *testing.T) {
	h := paneTestHome(t)

	// Walk the cursor onto alpha's second tab row (j: instance → tab 0 → tab 1).
	pressNav(t, h, "j")
	pressNav(t, h, "j")
	require.True(t, h.sidebar.GetSelection().IsTab)
	require.Equal(t, 1, h.store.ActiveTab())

	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes())
	terminalPane := h.store.OpenPanes()[0]
	assert.Equal(t, 1, terminalPane.Tab(), "the tree row's tab is what gets bound")

	// Jumping the selection tab must not move the open pane's binding —
	// and s on the OTHER tab of the same instance opens a second pane.
	h.focusRegion(layout.RegionTree)
	_, _ = h.handleTabJump(1)
	require.Equal(t, 0, h.store.ActiveTab())
	assert.Equal(t, 1, terminalPane.Tab(), "the pane's tab is bound independently of the selection")

	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes(), "each (instance, tab) pair is its own pane")
	assert.Equal(t, 0, h.store.OpenPanes()[1].Tab())
}

// TestPane_NumberJumpTargetsFocusedPaneNotSidebarSelection covers the sibling
// target-resolution path from #1253: digit jumps are routed while a pane is
// focused, so they must retarget that pane's tab binding instead of the
// sidebar-selected instance's active tab.
func TestPane_NumberJumpTargetsFocusedPaneNotSidebarSelection(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	paneA := h.store.OpenPanes()[0]
	require.Same(t, alpha, paneA.Instance())
	require.Equal(t, 0, paneA.Tab())

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	paneB := h.store.OpenPanes()[1]
	require.Same(t, beta, h.store.GetSelectedInstance())
	require.Same(t, beta, paneB.Instance())
	require.Equal(t, 0, h.store.ActiveTab())

	h.focusRegion(layout.PaneRegion(paneA.ID()))
	_, _ = h.handleTabJump(2)

	assert.Equal(t, 1, paneA.Tab(), "focused pane A must jump to tab 2")
	assert.Equal(t, 0, paneB.Tab(), "sidebar-selected pane B must not be retargeted")
	assert.Equal(t, 0, h.store.ActiveTab(), "sidebar active tab must stay on B's tab 1")
	assert.Same(t, beta, h.store.GetSelectedInstance(), "sidebar selection must stay on B")

	h.focusRegion(layout.RegionTree)
	_, _ = h.handleTabJump(2)

	assert.Equal(t, 1, h.store.ActiveTab(), "tree-focus jump must still target the sidebar selection")
	assert.Equal(t, 1, paneA.Tab(), "tree-focus jump must not mutate pane A further")
	assert.Equal(t, 0, paneB.Tab(), "tree-focus jump only changes the selection's active tab")
}

// TestPane_NumberJumpAnnotatesSelectedTabDivergence covers the #1289 tab-level
// mismatch while preserving #1255: when a pane-focused digit jump changes the
// visible pane tab, the sidebar-selected active tab is not retargeted, so the
// pane header must make that divergence explicit.
func TestPane_NumberJumpAnnotatesSelectedTabDivergence(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	paneB := h.store.OpenPanes()[0]
	require.Same(t, beta, paneB.Instance())
	require.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active())
	require.Equal(t, 0, h.store.ActiveTab())

	_, _ = h.handleTabJump(2)

	assert.Equal(t, 1, paneB.Tab(), "focused beta pane jumps to tab 2")
	assert.Equal(t, 0, h.store.ActiveTab(), "pane-focused jump must not retarget the sidebar selection")
	view := h.View()
	assert.Contains(t, view, "beta · › Terminal · selected: beta · ◆ Agent",
		"pane header shows the jumped tab and the still-selected tree tab")
	assert.Contains(t, view, "1 ◆ Agent *", "sidebar active-tab marker stays on the selected tab")
}

// TestPane_FocusRingCyclesNPanes: with three panes open, Tab cycles
// tree → pane 1 → pane 2 → pane 3 → automations → projects and wraps; Shift-Tab
// reverses; with no panes the ring is tree → automations → projects.
func TestPane_FocusRingCyclesNPanes(t *testing.T) {
	h := paneTestHome(t)

	// No panes: the ring is tree → automations → projects → tree.
	for _, want := range []string{layout.RegionAutomations, layout.RegionProjects, layout.RegionTree} {
		pressTab(t, h, false)
		require.Equal(t, want, h.ring.Active(), "without panes the ring is tree → automations → projects")
	}

	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.store.NumOpenPanes())
	require.Equal(t, 3, h.lastLayout.PaneCount(), "200 cols fits three panes")
	panes := h.store.OpenPanes()

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
		pressTab(t, h, false)
		require.Equal(t, want, h.ring.Active(), "Tab must cycle tree → panes in order → automations → projects")
	}
	backward := []string{
		layout.RegionProjects,
		layout.RegionAutomations,
		layout.PaneRegion(panes[2].ID()),
		layout.PaneRegion(panes[1].ID()),
		layout.PaneRegion(panes[0].ID()),
		layout.RegionTree,
	}
	for _, want := range backward {
		pressTab(t, h, true)
		require.Equal(t, want, h.ring.Active(), "Shift-Tab must cycle the same ring backwards")
	}
}

func TestPaneArrowKeysSwitchFocusedPaneAndClamp(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h))
	panes := h.store.OpenPanes()
	selectedBefore := h.store.GetSelectedInstance()

	h.focusRegion(layout.PaneRegion(panes[0].ID()))
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, layout.PaneRegion(panes[0].ID()), h.ring.Active(), "left edge clamps")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, layout.PaneRegion(panes[1].ID()), h.ring.Active(), "right moves to the next visible pane")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, layout.PaneRegion(panes[2].ID()), h.ring.Active())

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, layout.PaneRegion(panes[2].ID()), h.ring.Active(), "right edge clamps")

	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, layout.PaneRegion(panes[1].ID()), h.ring.Active(), "left moves to the previous visible pane")
	assert.Same(t, selectedBefore, h.store.GetSelectedInstance(), "pane focus switches must not retarget the tree selection")
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h), "switching focus must not reorder panes")

	h.focusRegion(layout.RegionTree)
	h.store.SetActiveTab(1) // Unopened terminal tab: isolate tree routing from #1493 open-pane focus.
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyLeft})
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "left/right on the tree stay sidebar navigation, not pane switching")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "right on the tree stays sidebar navigation, not pane switching")
}

func TestPaneArrowKeysMoveAfterCancelingPreview(t *testing.T) {
	h := paneTestHome(t)
	gamma := h.store.GetInstanceByTitle("gamma")

	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	panes := h.store.OpenPanes()
	require.Len(t, panes, 2)
	paneA := panes[0]
	paneB := panes[1]
	require.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active(), "precondition: beta's right pane is focused")

	h.sidebar.SetSelectedInstance(2)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "selecting unopened gamma while beta's pane is focused creates a transient preview")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyLeft})
	require.Nil(t, h.panePreviewTxn, "left cancels the preview")
	assert.Equal(t, layout.PaneRegion(paneA.ID()), h.ring.Active(), "left still moves focus to the previous pane")
	assert.Same(t, gamma, h.store.GetSelectedInstance(), "pane switching must not retarget the tree selection")

	h.sidebar.SetSelectedInstance(2)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "selecting unopened gamma while alpha's pane is focused creates a transient preview")
	_, _ = h.handleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	require.Nil(t, h.panePreviewTxn, "right cancels the preview")
	assert.Equal(t, layout.PaneRegion(paneB.ID()), h.ring.Active(), "right still moves focus to the next pane")
	assert.Same(t, gamma, h.store.GetSelectedInstance(), "pane switching must not retarget the tree selection")
}

// TestPane_AutoHideOnShrinkRestoreOnGrow drives the §2.6 pane-count fitting:
// shrinking below what fits auto-hides the least-recently-focused panes —
// bindings retained, focused pane always visible — and growing restores them
// in workspace order.
func TestPane_AutoHideOnShrinkRestoreOnGrow(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h))
	panes := h.store.OpenPanes()

	// Focus alpha's pane: recency is now alpha > gamma > beta.
	h.focusRegion(layout.PaneRegion(panes[0].ID()))

	// Two panes fit at 150 cols: beta (least recently focused) auto-hides;
	// the binding stays open and the survivors keep workspace order.
	resizeHome(h, 150, 40)
	require.Equal(t, 2, h.lastLayout.PaneCount())
	assert.Equal(t, []string{"alpha", "gamma"}, visibleTitles(h),
		"the least-recently-focused pane auto-hides first")
	assert.Equal(t, 3, h.store.NumOpenPanes(), "auto-hide retains the binding")
	assert.Equal(t, layout.PaneRegion(panes[0].ID()), h.ring.Active(),
		"the focused pane is never the one auto-hidden")

	// One pane below the multi-pane threshold: only the focused pane stays.
	resizeHome(h, layout.MultiPaneMinWidth-1, 40)
	require.Equal(t, 1, h.lastLayout.PaneCount())
	assert.Equal(t, []string{"alpha"}, visibleTitles(h))
	assert.Equal(t, 3, h.store.NumOpenPanes())

	// Growing back restores every pane, in workspace order.
	resizeHome(h, 200, 40)
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h),
		"grow restores the auto-hidden panes in order")
}

// TestPane_OpenBeyondCapacityAutoHidesLRU: opening one more pane than fits
// auto-hides the least-recently-focused pane instead of erroring (§2.6).
func TestPane_OpenBeyondCapacityAutoHidesLRU(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, 150, 40) // two panes fit

	for i := 0; i < 2; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, []string{"alpha", "beta"}, visibleTitles(h))

	// Opening gamma at capacity hides alpha (LRU) and shows the new pane.
	h.sidebar.SetSelectedInstance(2)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 3, h.store.NumOpenPanes(), "the third pane opens")
	assert.Equal(t, []string{"beta", "gamma"}, visibleTitles(h),
		"opening beyond capacity auto-hides the least-recently-focused pane")
	gamma := h.store.OpenPanes()[2]
	assert.Equal(t, layout.PaneRegion(gamma.ID()), h.ring.Active(), "the new pane is focused")
}

func TestPane_AutoHideShowsTransientStatus(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 40) // one pane fits

	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, []string{"alpha"}, visibleTitles(h))

	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")

	require.Equal(t, 2, h.store.NumOpenPanes(), "the second pane still opens")
	assert.Equal(t, []string{"beta"}, visibleTitles(h), "width pressure hides alpha and shows beta")
	assert.Equal(t, "alpha · ◆ Agent hidden — too narrow for 2 panes; resize wider or use `s` open pane",
		h.errBox.FullError())
}

// TestPane_DegradationLadderWithNPanes drives the resize ladder with panes
// open: minimal mode keeps exactly one pane, fallback renders the banner, and
// growing back restores all bindings.
func TestPane_DegradationLadderWithNPanes(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	require.Equal(t, 3, h.lastLayout.PaneCount())

	resizeHome(h, 59, 14)
	assert.False(t, h.lastLayout.AutomationsVisible, "minimal mode drops the strip")
	assert.Equal(t, 1, h.lastLayout.PaneCount(), "minimal mode keeps a single pane")
	requireViewSized(t, h.View(), 59, 14)

	resizeHome(h, 39, 9)
	require.True(t, h.lastLayout.Fallback)
	assert.Empty(t, h.visiblePanes)
	view := h.View()
	requireViewSized(t, view, 39, 9)
	assert.Contains(t, view, "Terminal too small")

	resizeHome(h, 200, 40)
	assert.Equal(t, 3, h.lastLayout.PaneCount(), "grow restores every open pane")
	assert.Equal(t, []string{"alpha", "beta", "gamma"}, visibleTitles(h))
}

// TestPane_WKeepsTabKillMeaning: `w` with a pane focused still means "kill
// the selection's active tab" (here the unclosable agent tab → friendly
// error), never "hide the pane" — that is `x` (§2.3).
func TestPane_WKeepsTabKillMeaning(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	tabsBefore := len(alpha.GetTabs())

	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes())
	pressKey(t, h, "w")

	assert.Equal(t, 1, h.store.NumOpenPanes(), "w must not hide the pane")
	assert.Equal(t, tabsBefore, len(alpha.GetTabs()), "the agent tab is never closed")
	assert.Contains(t, h.errBox.String(), "agent tab", "w on the agent tab surfaces the friendly error")
}

func TestPane_SnapshotInstanceRemovalPrunesPanes(t *testing.T) {
	h := paneTestHome(t)
	beta := h.store.GetInstanceByTitle("beta")
	gamma := h.store.GetInstanceByTitle("gamma")

	pressKey(t, h, "s") // alpha pane
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s") // beta pane
	require.Equal(t, 2, h.store.NumOpenPanes())
	// Focus alpha's pane so the prune also exercises the focused-pane case.
	h.focusRegion(layout.PaneRegion(h.store.OpenPanes()[0].ID()))

	// The daemon reports alpha gone.
	require.True(t, h.reconcileSnapshot([]session.InstanceData{
		beta.ToInstanceData(), gamma.ToInstanceData(),
	}))

	assert.Nil(t, h.store.GetInstanceByTitle("alpha"))
	require.Equal(t, 1, h.store.NumOpenPanes(),
		"the removed instance's pane is pruned in the same reconcile")
	assert.Equal(t, []string{"beta"}, visibleTitles(h))
	assert.Same(t, beta, h.store.OpenPanes()[0].Instance())
	assert.Equal(t, 1, h.lastLayout.PaneCount(), "the survivor re-fits the workspace")
	assert.Equal(t, layout.RegionTree, h.ring.Active(),
		"focus falls back cleanly off the pruned pane")
}

// TestPane_AllPanesPausedWhileAttached extends the #598 gate to N capture
// slots: with panes open and the user attached, selectionChanged must
// dispatch NO capture work.
func TestPane_AllPanesPausedWhileAttached(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	// Age the throttle so it cannot be what suppresses the captures.
	h.lastPaneCapture = make(map[int]time.Time)

	h.attached.Store(true)
	cmd := h.selectionChanged()
	assert.Nil(t, cmd,
		"selectionChanged must return nil while attached: every pane's capture "+
			"is gated behind the attached flag (#598), so nothing may queue "+
			"behind the user's detach key")
}

// TestPane_CaptureThrottled pins the RFC §5.2 contention lever: each pane's
// capture dispatch is floored at paneCaptureMinInterval, so raising that one
// constant degrades every pane's cadence without touching the tick.
func TestPane_CaptureThrottled(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	require.Equal(t, 1, h.lastLayout.PaneCount())

	h.lastPaneCapture = make(map[int]time.Time)
	require.NotNil(t, h.panesRefresh(false), "an aged throttle admits the capture")
	assert.Nil(t, h.panesRefresh(false),
		"a second dispatch inside paneCaptureMinInterval must be swallowed")
}

// TestPane_InstanceRemovedPrunesPanes: when a pane's instance leaves the
// projection (killed here or externally), the next tick closes that pane
// instead of rendering a dead session's last capture forever.
func TestPane_InstanceRemovedPrunesPanes(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	pressKey(t, h, "s")
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.store.RemoveInstanceByTitle("alpha")
	_ = h.selectionChanged()
	assert.Equal(t, 1, h.store.NumOpenPanes(),
		"the removed instance's pane must be pruned")
	assert.Equal(t, []string{"beta"}, visibleTitles(h))
}

// TestPane_FollowsSameTitleSwap: a #765 kill+recreate swap (same title,
// rebuilt pointer) re-points open-pane bindings onto the replacement, so open
// panes keep showing the live session.
func TestPane_FollowsSameTitleSwap(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s")
	p := h.store.OpenPanes()[0]
	require.Equal(t, "alpha", p.Instance().Title)

	rebuilt := instanceWithFakeBackend(t, "alpha")
	require.True(t, h.store.ReplaceInstanceByTitle("alpha", rebuilt))
	assert.Same(t, rebuilt, p.Instance(),
		"open-pane bindings must follow a same-title swap (#765 class)")
}

// TestPane_EnterAttachTargetFollowsFocusContext: for non-embeddable panes,
// keyboard Enter follows the same context rule as the embedded path. A focused
// pane owns Enter even when the sidebar selection points elsewhere (#1253);
// with tree focus, Enter falls back to the sidebar selection (#1233/#1236).
func TestPane_EnterAttachTargetFollowsFocusContext(t *testing.T) {
	resetDetachWatchdog(t)
	h := paneTestHome(t)
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// Open alpha's pane, then drive the tree selection to beta.
	pressKey(t, h, "s")
	p := h.store.OpenPanes()[0]
	require.Equal(t, "alpha", p.Instance().Title)
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.Equal(t, "beta", h.store.GetSelectedInstance().Title)
	h.cancelPanePreview(false)

	var attachedLabel, attachedTitle string
	swapAttachOverlayCallbackFn(t, func(m *home, target sessionActionTarget, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		attachedLabel, attachedTitle = label, target.title
		return m.attachOverlayCallback(target, label, traceSuffix, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch) // detach immediately — no real PTY
			return ch, nil
		})
	})

	// Focus alpha's pane: Enter must attach alpha, never silently retarget to
	// the sidebar-selected beta.
	h.focusRegion(layout.PaneRegion(p.ID()))
	_, cmd := h.handleEnter()
	require.NotNil(t, cmd, "the focused-pane attach must run")
	_ = runAttachTransitionCmd(t, h, cmd)
	assert.Equal(t, "handleEnter-pane", attachedLabel,
		"focused-pane Enter must use the pane attach path")
	assert.Equal(t, "alpha", attachedTitle,
		"Enter must target focused alpha, not sidebar-selected beta")
	endDetachWatchdog()

	// Detach restored everything: focus still on the pane, binding intact.
	assert.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active(), "detach restores focus")
	assert.Equal(t, 1, h.store.NumOpenPanes(), "detach keeps the pane open")
	assert.Equal(t, "alpha", h.store.OpenPanes()[0].Instance().Title)

	// Focus the tree: Enter attaches the sidebar selection.
	h.focusRegion(layout.RegionTree)
	_, cmd = h.handleEnter()
	require.NotNil(t, cmd)
	_ = runAttachTransitionCmd(t, h, cmd)
	assert.Equal(t, "handleEnter-sidebar", attachedLabel)
	assert.Equal(t, "beta", attachedTitle)
	endDetachWatchdog()
}

// TestPane_TreeNavFromPaneRefocusesTree: a tree-navigation key pressed while a
// pane holds the focus ring re-homes the ring on the tree, so the ring-reading
// attach verb `o` resolves the freshly selected instance rather than the stale
// focused pane (#1233 adjacent-audit fix; focusTreeForNav).
func TestPane_TreeNavFromPaneRefocusesTree(t *testing.T) {
	h := paneTestHome(t)
	pressKey(t, h, "s") // open alpha's pane and focus it
	p := h.store.OpenPanes()[0]
	require.Equal(t, layout.PaneRegion(p.ID()), h.ring.Active(), "the opened pane holds focus")

	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyDown}, keys.KeyDown)
	assert.Equal(t, layout.RegionTree, h.ring.Active(),
		"tree navigation must return focus to the tree so o/attach resolves the selection")
}

// TestPane_HideMiddleFocusesSuccessor: hiding a middle pane lands focus on
// the pane that takes its slot, and the remaining panes keep workspace order.
func TestPane_HideMiddleFocusesSuccessor(t *testing.T) {
	h := paneTestHome(t)
	for i := 0; i < 3; i++ {
		h.sidebar.SetSelectedInstance(i)
		_ = h.selectionChanged()
		pressKey(t, h, "s")
	}
	panes := h.store.OpenPanes()

	h.focusRegion(layout.PaneRegion(panes[1].ID()))
	pressKey(t, h, "x")
	assert.Equal(t, []string{"alpha", "gamma"}, visibleTitles(h))
	assert.Equal(t, layout.PaneRegion(h.store.OpenPanes()[1].ID()), h.ring.Active(),
		"focus lands on the pane that took the hidden pane's slot")
}

// TestE2E_PaneFlow drives the real tea.Program through the pane lifecycle:
// s opens a focused pane, the tree walks to another instance and s opens a
// second pane beside it, Tab cycles the ring across both, and x hides the
// focused pane with focus landing on the survivor.
func TestE2E_PaneFlow(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("alpha")
	eh.addStartedInstance("beta")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	paneState := func() (count int, titles []string, region string) {
		eh.query(func(h *home) {
			count = h.store.NumOpenPanes()
			titles = visibleTitles(h)
			region = h.ring.Active()
		})
		return
	}

	// s opens the selection (alpha) as a focused pane.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens alpha's pane focused", func() bool {
		count, titles, region := paneState()
		return count == 1 && len(titles) == 1 && titles[0] == "alpha" && layout.IsPaneRegion(region)
	})

	// The tree walks to beta (j walks alpha → its two tab rows → beta);
	// alpha's pane stays put. Then s opens beta beside it.
	for i := 0; i < 3; i++ {
		eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	eh.waitUntil(e2eAsyncTimeout, "tree selection lands on beta", func() bool {
		var selected string
		eh.query(func(h *home) {
			if s := h.store.GetSelectedInstance(); s != nil {
				selected = s.Title
			}
		})
		return selected == "beta"
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	eh.waitUntil(e2eAsyncTimeout, "s opens beta's pane to the right", func() bool {
		count, titles, _ := paneState()
		return count == 2 && len(titles) == 2 && titles[0] == "alpha" && titles[1] == "beta"
	})

	// Both pane headers render side by side.
	var view string
	eh.query(func(h *home) { view = h.View() })
	// Pane 2's tab label is whatever the shared active-tab index resolved to
	// after the tree walk (the index survives instance switches by design),
	// so assert the instance halves only.
	assert.Contains(t, view, "alpha · ◆ Agent", "pane 1 header shows its binding")
	assert.Contains(t, view, "beta · ", "pane 2 header shows its binding")

	// Tab from the beta pane wraps via automations/projects/tree back around to
	// the alpha pane; assert the ring visits a pane region again.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	eh.waitUntil(e2eAsyncTimeout, "the focus ring cycles back to a pane", func() bool {
		_, _, region := paneState()
		return layout.IsPaneRegion(region)
	})

	// x hides the focused pane; the survivor keeps focus and the workspace.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	eh.waitUntil(e2eAsyncTimeout, "x hides the focused pane", func() bool {
		count, titles, region := paneState()
		return count == 1 && len(titles) == 1 && layout.IsPaneRegion(region)
	})
}

// TestPane_StoreOpenPanePrimitives unit-tests the store's open-pane list:
// dedupe lookup, ordered append, close, and the recency-ranked visibility
// pick that drives the §2.6 auto-hide.
func TestPane_StoreOpenPanePrimitives(t *testing.T) {
	proj := store.NewProjection()
	a := instanceWithFakeBackend(t, "a")
	b := instanceWithFakeBackend(t, "b")
	proj.AddInstance(a)
	proj.AddInstance(b)

	require.Nil(t, proj.AddOpenPane(nil, 0), "nil instances never open")

	p1 := proj.AddOpenPane(a, 0)
	p2 := proj.AddOpenPane(a, 1)
	p3 := proj.AddOpenPane(b, 0)
	require.Equal(t, 3, proj.NumOpenPanes())
	assert.Same(t, p2, proj.FindOpenPane(a, 1))
	assert.Nil(t, proj.FindOpenPane(b, 1))
	assert.NotEqual(t, p1.ID(), p2.ID(), "pane ids are unique")

	// Visibility: all fit → workspace order regardless of recency.
	proj.TouchOpenPane(p1)
	vis := proj.VisibleOpenPanes(3)
	require.Equal(t, []*store.OpenPane{p1, p2, p3}, vis)

	// Two fit: p2 is now least recently focused (p3 opened after it, p1
	// touched last) → p2 hides, order preserved.
	vis = proj.VisibleOpenPanes(2)
	require.Equal(t, []*store.OpenPane{p1, p3}, vis)

	// One fits: only the most recently focused survives.
	vis = proj.VisibleOpenPanes(1)
	require.Equal(t, []*store.OpenPane{p1}, vis)
	assert.Empty(t, proj.VisibleOpenPanes(0))

	require.True(t, proj.RebindOpenPane(p1, b, 1))
	assert.Same(t, b, p1.Instance())
	assert.Equal(t, 1, p1.Tab())
	assert.Same(t, p1, proj.FindOpenPane(b, 1))

	require.True(t, proj.CloseOpenPane(p2))
	require.False(t, proj.CloseOpenPane(p2), "closing twice reports absence")
	assert.Equal(t, 2, proj.NumOpenPanes())
}

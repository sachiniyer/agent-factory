package app

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Cross-session pane preview vs the digit-jump verbs (#1884 close-out): the
// last pane-focused tab verb (`1`-`9` in handleTabJump, and the `g` prompt in
// jumpTargetTabs) must resolve the on-screen binding — effectivePaneBinding —
// before cancelling the preview, the same way `w`/`R`/`</>` already do. A pane
// engaged in a #1321 cross-session preview RENDERS a different session's tab
// while its committed binding stays hidden underneath, so reading the raw
// committed binding retargeted a session the user could not see.
// ----------------------------------------------------------------------------

// crossSessionPreviewHome builds a home with two multi-tab instances, alpha and
// beta, each carrying agent + three shell tabs (slot 0..3) with stable ids, with
// the selection on alpha and a pane-size layout. It is the cross-session shape
// the digit-jump wrong-target bug needs: the focused pane's committed session
// must differ from the session whose tab it renders via a transient preview.
func crossSessionPreviewHome(t *testing.T) (*home, *session.Instance, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	tabIDs := []string{"id-agent", "id-shell", "id-shell2", "id-shell3"}
	mk := func(title string) *session.Instance {
		inst := instanceWithFakeBackend(t, title)
		inst.AddTabForTest("agent", session.TabKindAgent)
		inst.AddTabForTest("shell", session.TabKindShell)
		inst.AddTabForTest("shell-2", session.TabKindShell)
		inst.AddTabForTest("shell-3", session.TabKindShell)
		for i, tab := range inst.GetTabs() {
			tab.ID = tabIDs[i]
		}
		return inst
	}
	alpha := mk("alpha")
	beta := mk("beta")
	h.store.AddInstance(alpha)
	h.store.AddInstance(beta)
	h.sidebar.SetSelectedInstance(0)
	_ = h.selectionChanged()
	resizeHome(h, 200, 40)
	return h, alpha, beta
}

// armCrossSessionPreview parks the tree cursor on beta's tab row idx (expanding
// beta first) while the alpha pane holds focus, arming a live cross-session
// preview over the focused pane. The pane RENDERS (beta, idx) while its
// committed binding stays (alpha, 1) — the on-screen/committed duality the
// sibling verbs already resolve.
func armCrossSessionPreview(t *testing.T, h *home, alpha, beta *session.Instance, focused *store.OpenPane, idx int) {
	t.Helper()
	// Expand beta by selecting its instance row, then drop the cursor onto its
	// tab row; the store selection follows the cursor while the panes keep
	// their own bindings (the real path: sidebar navigation while a pane holds
	// focus).
	h.sidebar.SelectInstance(beta)
	_ = h.selectionChanged()
	h.sidebar.SelectTabRow(beta.Title, idx)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a cross-session preview is live")
	require.Same(t, beta, h.panePreviewTxn.target.instance,
		"precondition: the focused pane RENDERS beta")
	require.Equal(t, idx, h.panePreviewTxn.target.tab,
		"precondition: the focused pane renders beta tab idx")
	require.Same(t, alpha, focused.Instance(),
		"precondition: but the committed binding is still alpha")
	require.Equal(t, 1, focused.Tab(),
		"precondition: the committed tab is still alpha slot 1")
}

// TestTabJump_PreviewingPaneTargetsOnScreenInstance is the headline repro for
// the digit-jump wrong-target class (#1884 close-out): with the focused pane
// committed to alpha while previewing a DIFFERENT session's tab, a digit jump
// must retarget the session the user is LOOKING at (the preview's session) —
// never the hidden committed session. The sibling verbs (w/R/</>) resolve the
// on-screen binding first; before the fix `1`-`9` read the committed binding and
// silently retargeted a session that appeared nowhere on screen.
func TestTabJump_PreviewingPaneTargetsOnScreenInstance(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane(), "precondition: pane holds focus")

	// Preview beta tab 1 (shell) on the focused (alpha) pane.
	armCrossSessionPreview(t, h, alpha, beta, focused, 1)

	// Press 3 (oneBased) -> idx 2 (shell-2), a DIFFERENT tab than the previewed
	// one. Expected: the pane rebinds to beta tab 2 — a jump WITHIN the
	// on-screen session, not a retarget onto the hidden committed session.
	_, _ = h.handleTabJump(3)

	assert.Same(t, beta, focused.Instance(),
		"the digit jump must retarget the on-screen session (beta), not the hidden committed one (alpha)")
	assert.Equal(t, 2, focused.Tab(),
		"the digit jump must land on the on-screen instance's tab 3 (slot 2)")
	assert.Nil(t, h.panePreviewTxn, "the preview must be consumed, not left dangling")
}

// TestTabJump_PreviewingPaneCommitsPreviewedTab pins the matched-digit case:
// when the digit names the very tab being previewed, the jump must commit that
// preview (rebind to the previewed (instance, tab)) — the digit-form of `S`/Enter
// committing the preview target.
func TestTabJump_PreviewingPaneCommitsPreviewedTab(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())
	armCrossSessionPreview(t, h, alpha, beta, focused, 2) // preview beta tab 2

	// Press 3 (oneBased) -> idx 2, the previewed tab.
	_, _ = h.handleTabJump(3)

	assert.Same(t, beta, focused.Instance(),
		"the digit jump commits the preview: pane rebinds to the on-screen session")
	assert.Equal(t, 2, focused.Tab())
	assert.Nil(t, h.panePreviewTxn)
}

// TestTabJump_PreviewingPaneFocusesExistingPaneOfOnScreenInstance pins the
// open-or-focus contract on the cross-session path (#3267): with a SECOND pane
// already showing the on-screen session's target tab, the digit jump must focus
// THAT pane, never rebind the focused (previewing) one — and never miss the
// target by searching the committed session's roster. Before the fix,
// FindOpenPane(p.Instance(), idx) searched alpha's roster, missed the beta pane
// already showing the tab, and silently rebound the previewing pane.
func TestTabJump_PreviewingPaneFocusesExistingPaneOfOnScreenInstance(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	// The focused pane previews beta tab 1 (shell), committed to alpha tab 1.
	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())

	// A SECOND pane already shows beta tab 2 (shell-2) — the open-or-focus
	// target for a press of `3`. Opening it moves focus onto it; re-focus the
	// alpha pane so the digit jump dispatches from the previewing pane.
	existing := openTestPane(t, h, beta, 2)
	require.NotSame(t, focused, existing)
	h.focusRegion(layout.PaneRegion(focused.ID()))
	require.Same(t, focused, h.focusedOpenPane())

	// Arm the cross-session preview over the focused (alpha) pane for beta tab 1.
	// beta tab 2 has a pane, but the PREVIEW target is beta tab 1, so arming
	// proceeds (no already-open tab at the preview target to steal focus).
	armCrossSessionPreview(t, h, alpha, beta, focused, 1)

	_, _ = h.handleTabJump(3) // idx 2 — open in the beta pane

	assert.Same(t, existing, h.focusedOpenPane(),
		"the jump must focus the pane already showing beta tab 3 (the open-or-focus contract)")
	assert.Same(t, alpha, focused.Instance(),
		"the focused (previewing) pane must keep its committed binding — it was not rebound onto an already-open tab")
	assert.Equal(t, 1, focused.Tab())
}

// TestTabJump_PreviewingPaneRefusalNamesOnScreenInstance pins the refusal
// message (#7700): when the on-screen session has no tab N, the jump must
// refuse naming the session the user is LOOKING at, not the hidden committed
// session. Before the fix, tabJumpRefusal(p.Instance(), …) printed the hidden
// session's title while the visible one had the tab the user asked for — the
// "names a binding the user cannot see" signature of #1884.
func TestTabJump_PreviewingPaneRefusalNamesOnScreenInstance(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())
	armCrossSessionPreview(t, h, alpha, beta, focused, 1)

	// alpha has 4 tabs (0..3) and so does beta (0..3). Press 9 (out of range for
	// both) — the refusal must name beta (the on-screen session).
	_, _ = h.handleTabJump(9)

	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), `session "beta" has no tab 9`,
		"the refusal must name the on-screen session (beta), not the hidden committed session (alpha)")
	require.NotContains(t, h.errBox.String(), "alpha",
		"the refusal must never name the hidden committed session")
}

// TestJumpTabPrompt_ListsOnScreenInstanceTabs pins the lockstep between the `g`
// prompt (jumpTargetTabs) and the digit jump (handleTabJump): while a focused
// pane previews a DIFFERENT session, the prompt must resolve its tab list
// against the on-screen (effective) binding — the same target the digit jump
// acts on — never the hidden committed binding. Before the fix, jumpTargetTabs
// read the raw committed binding and listed alpha's tabs while `3` jumped beta,
// so the prompt and the digits could land in different sessions.
func TestJumpTabPrompt_ListsOnScreenInstanceTabs(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())
	armCrossSessionPreview(t, h, alpha, beta, focused, 1) // pane renders beta tab 1

	// The prompt's tab list must describe beta (the on-screen session), not
	// alpha (the hidden committed one). alpha and beta carry identical names
	// and ids by construction, so distinguish by pointer identity: each entry
	// must be beta's slot, never alpha's.
	tabs := h.jumpTargetTabs()
	require.Len(t, tabs, 4, "beta has 4 tabs")
	betaTabs := beta.GetTabs()
	alphaTabs := alpha.GetTabs()
	for i := range tabs {
		require.Same(t, betaTabs[i], tabs[i], "tab %d must come from beta's roster", i)
		require.NotSame(t, alphaTabs[i], tabs[i], "tab %d must not come from alpha's roster", i)
	}

	// Type "3" and submit: the resolver should land on beta tab 2 (shell-2), and
	// the dispatch through handleTabJump must rebind the focused pane to beta.
	_, _ = h.showJumpTabPrompt()
	require.NotNil(t, h.promptOverlay, "precondition: the jump prompt opened")
	_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	_, _ = h.handleStateJumpTab(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Same(t, beta, focused.Instance(),
		"the g prompt jump must retarget the on-screen session (beta), agreeing with the digit jump")
	assert.Equal(t, 2, focused.Tab(),
		"the g prompt must land on beta tab 3 (slot 2) — shell-2")
	assert.Nil(t, h.panePreviewTxn, "the preview must be consumed")
}

// TestTabJump_SameInstancePreviewUnchanged is the regression guard: a
// SAME-instance preview (the common case, where the previewed tab and the
// committed binding belong to the same session) must keep working as before —
// the digit jump rebinds the pane in place via JumpToTab, with no cross-session
// rebind path. The fix branches only when the on-screen instance differs from
// the committed one.
func TestTabJump_SameInstancePreviewUnchanged(t *testing.T) {
	h, alpha, _ := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())

	// Same-instance preview: the cursor on alpha tab 2 (shell-2) previews alpha
	// tab 2 over the alpha pane committed to alpha tab 1.
	h.sidebar.SelectTabRow(alpha.Title, 2)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a same-instance preview is live")
	require.Same(t, alpha, h.panePreviewTxn.target.instance)
	require.Equal(t, 2, h.panePreviewTxn.target.tab)
	require.Same(t, alpha, focused.Instance(), "precondition: committed binding is the same instance")

	// Press 3 (slot 2, shell-2) — same as the previewed tab. The pane must jump
	// in place to alpha tab 2.
	_, _ = h.handleTabJump(3)

	assert.Same(t, alpha, focused.Instance(),
		"a same-instance jump keeps the committed instance")
	assert.Equal(t, 2, focused.Tab(), "the pane jumps to alpha tab 3 (slot 2)")
	assert.Nil(t, h.panePreviewTxn)
}

// TestTabJump_PreviewingPaneRefusesWhenOnScreenSessionLacksTab pins the boundary
// between the open-or-focus branch and the refusal branch on the cross-session
// path: a digit naming a tab that does not exist in the on-screen session must
// refuse (naming the on-screen session), and must never fall through to the
// committed binding's roster.
func TestTabJump_PreviewingPaneRefusesWhenOnScreenSessionLacksTab(t *testing.T) {
	h, alpha, beta := crossSessionPreviewHome(t)

	focused := openTestPane(t, h, alpha, 1)
	require.Equal(t, focused, h.focusedOpenPane())
	armCrossSessionPreview(t, h, alpha, beta, focused, 1)

	// Press 5 (oneBased) -> idx 4. beta has tabs 0..3 (4 tabs), so idx 4 is out
	// of range; the refusal must name beta.
	_, _ = h.handleTabJump(5)

	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), `session "beta" has no tab 5`,
		"the refusal must name the on-screen session (beta), which lacks tab 5")
	assert.Same(t, alpha, focused.Instance(),
		"a refused jump must not rebind the focused pane onto the committed session")
	assert.Equal(t, 1, focused.Tab(), "the committed binding is untouched")
}

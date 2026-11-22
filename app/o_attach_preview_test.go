package app

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
)

// TestAttachOnPreviewingPaneAttachesThePreviewTarget is the #1321 wrong-target
// regression for the full-screen attach verb `o`. A workspace pane committed to
// session alpha can transiently RENDER a preview of session beta — the
// panePreviewTxn makes the pane's DISPLAYED (instance, tab) diverge from its
// committed p.Instance()/p.Tab() binding. Every sibling verb that acts on "the
// thing on screen" resolves the preview first (Enter commits it; `w` reads the
// effective binding), but `o`'s focused-pane fast path routed straight to
// handleEnterPane, which read the raw committed binding. So `o` on a pane
// visibly showing beta silently full-screen-attached alpha — the session the
// user could not see — while Enter on the identical state committed the preview
// and entered beta. The two attach-flavored verbs disagreed.
//
// This builds the divergent state with production mechanisms only (`s` opens the
// pane; selectionChanged installs the preview), leaves the focus ring on the pane
// (the mouse-reachable state a header click produces, which the keyboard path
// can't reach because tree-nav re-homes the ring), and drives `o` through the
// real Update dispatch. The fix resolves the preview before attaching, so `o`
// attaches beta and commits the txn — matching Enter.
func TestAttachOnPreviewingPaneAttachesThePreviewTarget(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// `s` opens the selection (alpha, tab 0) as a focused pane — the ring
	// rests on it, the precondition the keyboard-only path can't reach but a
	// pane-header click (or an idle preview tick) can.
	pressKey(t, h, "s")
	require.Equal(t, 1, h.store.NumOpenPanes(), "precondition: alpha's pane is open")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: the pane is committed to alpha")
	require.Equal(t, layout.PaneRegion(alphaPane.ID()), h.ring.Active(),
		"precondition: the focus ring rests on the pane, not the tree")

	// Move the sidebar cursor onto beta — selectionChanged installs a live
	// preview of beta in alpha's pane, the real path, not by poking the txn.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a preview is live")
	require.Equal(t, alphaPane.ID(), h.panePreviewTxn.ownerPaneID,
		"precondition: the focused pane owns it")
	require.Same(t, beta, h.panePreviewTxn.target.instance,
		"precondition: the preview targets beta")
	require.Same(t, alpha, alphaPane.Instance(),
		"precondition: but the committed binding is still alpha — the duality")
	require.True(t, h.paneIsPreviewing(alphaPane),
		"precondition: the focused pane is previewing")

	// Observe the attach target the way driveAttachKey does
	// (search_close_focus_ring_test.go): the swapped callback captures the
	// sessionActionTarget.title the attach was dispatched for, without
	// standing up a real WS PTY stream.
	var attachedTitle string
	swapAttachOverlayCallbackFn(t, func(_ *home, target sessionActionTarget, _, _ string, _ func() (chan struct{}, error)) tea.Cmd {
		attachedTitle = target.title
		return nil
	})
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()),
		"first-time attach help must be seen so the attach callback runs synchronously inside the transition")

	// `o` via the real Update dispatch — the path the real key takes.
	mod, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	h = mod.(*home)
	runAttachTransitionCmd(t, h, cmd)

	require.Equal(t, "beta", attachedTitle,
		"`o` on a pane previewing beta must attach beta (the session on screen), "+
			"not the committed alpha — the same rule Enter applies by committing the preview first")
	require.Nil(t, h.panePreviewTxn,
		"`o` must commit the preview txn, matching Enter")
	require.Same(t, beta, alphaPane.Instance(),
		"the pane is now bound to the preview target beta (the commit rebinds it before attach)")
}

// TestAttachOnPreviewingPane_AttachesThePreviewTarget_MouseHeaderClick reaches
// the divergent state the way the bug report says a user actually reaches it: a
// pane-header click focuses the previewing pane WITHOUT committing or cancelling
// the preview (focusRegionClick only moves the ring), leaving the ring on a pane
// that owns a live preview of beta while staying committed to alpha. Pressing `o`
// must attach beta, the session on screen — the headline mouse-works state of
// #1819/#1321. This is the tab_multitab_test.go:722-756 setup with `o` instead of `w`.
func TestAttachOnPreviewingPane_AttachesThePreviewTarget_MouseHeaderClick(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")

	// Open alpha's pane (ring on it), then re-home the ring to the tree so a
	// tree-row selection installs the preview WITHOUT the ring moving.
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: committed to alpha")
	h.focusRegion(layout.RegionTree)
	require.Equal(t, layout.RegionTree, h.ring.Active(), "precondition: ring on the tree")

	// Cursor onto beta installs a preview on alpha's pane (previewOwnerPane =
	// last-focused pane); the ring stays on the tree.
	h.sidebar.SetSelectedInstance(1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: preview is live")
	require.Equal(t, alphaPane.ID(), h.panePreviewTxn.ownerPaneID, "precondition: alpha pane owns it")
	require.Same(t, beta, h.panePreviewTxn.target.instance, "precondition: preview targets beta")

	// THE BUG-REPORT GESTURE: click the pane's HEADER to focus it. Unlike every
	// keyboard path this neither commits nor cancels the preview, leaving the
	// divergent state under a focused pane — the exact mouse-works signature.
	runHermeticCmd(t, h, clickZone(t, h, zones.PaneHeader(layout.PaneRegion(alphaPane.ID()))), 0)
	require.Equal(t, alphaPane, h.focusedOpenPane(), "precondition: the header click focused the pane")
	require.NotNil(t, h.panePreviewTxn, "precondition: the header click left the preview live")
	require.Same(t, alpha, alphaPane.Instance(), "precondition: committed binding is still alpha")

	var attachedTitle string
	swapAttachOverlayCallbackFn(t, func(_ *home, target sessionActionTarget, _, _ string, _ func() (chan struct{}, error)) tea.Cmd {
		attachedTitle = target.title
		return nil
	})
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	mod, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	h = mod.(*home)
	runAttachTransitionCmd(t, h, cmd)

	assert.Equal(t, "beta", attachedTitle,
		"`o` after a pane-header click must attach the visible preview target beta, "+
			"not the committed alpha — the mouse-reachable wrong-target state (#1819)")
	assert.Nil(t, h.panePreviewTxn, "`o` commits the preview, like Enter")
	assert.Same(t, beta, alphaPane.Instance(), "the pane is rebound onto beta")
}

// TestAttachOnPreviewingPane_RestingTargetRestores verifies guarantee G5: when
// the focused pane previews a RESTING (Lost/Dead/archived) row, `o` restores it
// (mirroring handleEnter's preview branch and the tree-row `o` path) rather than
// surfacing previewCommitError's "cannot commit preview" fence. `o`, Enter, and
// the tree-row `o` path all agree on a resting preview target.
func TestAttachOnPreviewingPane_RestingTargetRestores(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	// A resting (Dead) row that the focused pane will preview.
	resting := newDeadInstance(t, "resting-session", session.Dead)
	h.store.AddInstance(resting)

	// Open alpha's pane, then cursor onto the resting row so the preview
	// installs in alpha's pane (changePreviewPostRestore path).
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: committed to alpha")
	h.sidebar.SetSelectedInstance(h.store.NumInstances() - 1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a preview is live")
	require.Same(t, resting, h.panePreviewTxn.target.instance,
		"precondition: the preview targets the resting row")

	// Stub the restore RPC so the restore dispatches hermetically.
	var called bool
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(daemon.RestoreSessionRequest) (string, error) {
		called = true
		return "/p", nil
	}
	t.Cleanup(func() { restoreSessionThroughDaemon = prev })

	// `o` via the real handler (the dispatch path `Update` takes for `o`); the
	// restore cmd is returned unwrapped at this layer, matching
	// TestHandleAttach_RestingRowRestores' pattern.
	mod, cmd := h.handleAttach()
	h = mod.(*home)

	require.Equal(t, session.OpRestoring, resting.GetInFlightOp(),
		"`o` on a pane previewing a resting row must restore it (#2489), raising the optimistic op")
	require.False(t, h.attached.Load(), "`o` on a resting preview target must restore, not attach")
	require.Nil(t, h.panePreviewTxn, "the restore resolved the preview txn")
	require.NotNil(t, cmd, "the restore must dispatch its daemon command")
	// The restore RPC runs lazily inside the cmd (off the event loop), exactly
	// like handleEnter's path — invoke it to confirm it dials the daemon.
	done, ok := cmd().(instanceRestoredMsg)
	require.True(t, ok, "the command must emit instanceRestoredMsg")
	require.NoError(t, done.err)
	require.True(t, called, "the restore RPC must fire")
}

// TestAttachOnPreviewingPane_RemoteTargetAttachesOnFirstPress verifies
// guarantee G11: `o` on a focused pane previewing a REMOTE (non-embeddable)
// session commits the preview then full-screen-attaches the remote target on
// the FIRST press — mirroring TestHandleEnter_RemotePanePreviewTriggersFullScreenAttach
// for Enter (#1601).
func TestAttachOnPreviewingPane_RemoteTargetAttachesOnFirstPress(t *testing.T) {
	resetDetachWatchdog(t)

	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	// A remote (non-embeddable) instance — liveSessionName == "" for its active
	// tab, so committing the preview must fall through to a full-screen attach.
	remote := instanceWithFakeBackend(t, "remote-inst")
	remote.SetBackend(remoteFakeBackend{session.NewFakeBackend()})
	remote.SetStatusForTest(session.Running)
	remote.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(remote)

	resizeHome(h, 200, 40)

	// Open alpha's pane, cursor onto the remote installs a preview of it.
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: committed to alpha")
	h.sidebar.SetSelectedInstance(h.store.NumInstances() - 1)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a preview is live")
	require.Same(t, remote, h.panePreviewTxn.target.instance,
		"precondition: the preview targets the remote instance")

	// Observe the same side effect TestHandleEnter_RemotePanePreviewTriggersFullScreenAttach
	// uses: the post-detach terminal mode re-assert lands in a buffer only a
	// real full-screen attach+detach produces.
	var out bytes.Buffer
	swapRemoteDetachResetWriter(t, &out)
	swapAttachOverlayCallbackFn(t, func(m *home, target sessionActionTarget, label, traceSuffix string, _ func() (chan struct{}, error)) tea.Cmd {
		return m.attachOverlayCallback(target, label, traceSuffix, func() (chan struct{}, error) {
			ch := make(chan struct{})
			close(ch)
			return ch, nil
		})
	})
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	// FIRST `o`, via the real key dispatch.
	mod, cmd := h.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	h = mod.(*home)
	require.Nil(t, h.panePreviewTxn, "`o` commits the preview target before attaching")
	cmd = runAttachTransitionCmd(t, h, cmd)
	endDetachWatchdog()

	require.NotNil(t, cmd,
		"the FIRST `o` must fire the full-screen attach — not require a second press (#1601)")
	require.Equal(t, remoteDetachTerminalReassert, out.String(),
		"`o` on a remote-previewing pane must run the remote full-screen attach+detach lifecycle")
}

// TestAttachOnPreviewingPane_BrowserOnlyTargetShowsGuardNotAttach verifies
// guarantee G12: `o` on a focused pane previewing a browser-only (web/vscode/docs)
// tab commits the preview (rebinding the pane to that tab) then surfaces
// handleEnterPane's webTabAttachGuard "view it in the web UI" message — no WS PTY
// dial — mirroring TestAttachFocusedBrowserOnlyPaneShowsGuardNotAttach for the
// no-preview case and TestClickCommittingWebTabPreviewShowsGuardNotAttach for the
// click path.
func TestAttachOnPreviewingPane_BrowserOnlyTargetShowsGuardNotAttach(t *testing.T) {
	h := paneTestHome(t)
	alpha := h.store.GetInstanceByTitle("alpha")

	// A second instance carrying a web tab that the focused pane will preview.
	webInst := instanceWithFakeBackend(t, "web-inst")
	webInst.AddTabForTest("agent", session.TabKindAgent)
	webInst.AddWebTabForTest("web", "http://localhost:3000")
	webTabs := webInst.GetTabs()
	webTab := webTabs[len(webTabs)-1]
	require.Equal(t, session.TabKindWeb, webTab.Kind)
	webTabIdx := len(webTabs) - 1
	h.store.AddInstance(webInst)
	resizeHome(h, 200, 40)

	// Open alpha's pane, cursor onto the web instance so the preview installs.
	pressKey(t, h, "s")
	alphaPane := h.store.OpenPanes()[0]
	require.Same(t, alpha, alphaPane.Instance(), "precondition: committed to alpha")
	h.sidebar.SetSelectedInstance(h.store.NumInstances() - 1)
	h.store.SetActiveTab(webTabIdx)
	_ = h.selectionChanged()
	require.NotNil(t, h.panePreviewTxn, "precondition: a preview is live")
	require.Same(t, webInst, h.panePreviewTxn.target.instance,
		"precondition: the preview targets the web instance")

	// Safety net: a browser-only target must never dial a real WS PTY stream.
	t.Cleanup(SetAttachStreamFnForTest(
		func(context.Context, apiclient.StreamSession, string, int) (chan struct{}, error) {
			t.Errorf("a browser-only preview target must never dial a WS PTY stream")
			return nil, fmt.Errorf("unexpected dial")
		}))

	// `o` via the real handler.
	_, _ = h.handleAttach()

	assert.Contains(t, h.errBox.FullError(), "web tab",
		"the user must be pointed at the web UI, not handed a PTY-attach failure")
	assert.False(t, h.attachTransitioning,
		"a web tab has no PTY: `o` must never start a full-screen attach")
	assert.False(t, h.attached.Load())
	assert.Nil(t, h.panePreviewTxn, "the preview was committed (rebound) before the guard ran")
}

// TestAttachOnPreviewingPane_RetainsAutoHideNoticeClearTimer is the #2639
// follow-up for the attach verb. When the preview target is already open in
// another pane and only one pane fits, commitPanePreviewReplace focuses the
// target, auto-hides the preview owner, and synchronously installs the
// auto-hide notice; its returned cmd batches that notice's 3-second clear timer
// WITH a panesRefresh capture. The capture was computed against attached=false
// and would contend with the attach, so handleAttach drops it — but it must
// retain the notice-clear timer, or the notice lingers indefinitely (the
// post-detach repaint does not re-create a timer for the already-consumed
// notice). The fix re-arms the timer from the id the commit installed.
func TestAttachOnPreviewingPane_RetainsAutoHideNoticeClearTimer(t *testing.T) {
	h := paneTestHome(t)
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // only one pane fits

	visible, hidden := openPanesForBothAndReturnHidden(t, h)
	owner := h.store.FindOpenPane(visible, 0)
	target := h.store.FindOpenPane(hidden, 0)
	require.NotNil(t, owner)
	require.NotNil(t, target)
	require.NotSame(t, owner, target)

	w := h.paneWindows[owner.ID()]
	require.NotNil(t, w)
	original := paneBinding{instance: visible, tab: 0}
	preview := paneBinding{instance: hidden, tab: 0}
	seq := w.SetPreview(preview.instance, preview.tab, paneBindingLabel(original))
	h.panePreviewTxn = &panePreviewTxn{
		ownerPaneID: owner.ID(),
		original:    original,
		target:      preview,
		seq:         seq,
	}
	// Focus the owning pane so handleAttach's focused-pane fast path takes the
	// preview branch (a header-click state the keyboard path can't reach).
	h.focusRegion(layout.PaneRegion(owner.ID()))
	require.Same(t, owner, h.focusedOpenPane(), "precondition: the owner is focused")
	require.True(t, h.paneIsPreviewing(owner), "precondition: the owner is previewing")

	// Reset the notice left by setup so only the commit relayout is measured.
	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Stub the attach so handleEnterPane's attach cmd is harmless; the help
	// screen is marked seen so attachInstanceTab runs the transition cmd directly.
	swapAttachOverlayCallbackFn(t, func(_ *home, _ sessionActionTarget, _, _ string, _ func() (chan struct{}, error)) tea.Cmd {
		return nil
	})
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	_, cmd := h.handleAttach()

	require.NotEmpty(t, h.errBox.FullError(),
		"the commit relayout auto-hides the preview owner and shows the notice")
	noticeID := h.paneAutoHideNoticeID
	require.NotZero(t, noticeID, "the commit installed the auto-hide notice")

	// The attach drops the panesRefresh capture but MUST retain the notice's
	// 3-second clear timer — drain the batched cmd (past the timer's 3s wait)
	// and confirm a hideErrMsg for the installed notice is produced.
	var sawClearTimer bool
	for _, msg := range drainCmd(t, cmd, 4*time.Second) {
		if hide, ok := msg.(hideErrMsg); ok && hide.noticeID == noticeID {
			sawClearTimer = true
		}
	}
	require.True(t, sawClearTimer,
		"handleAttach must retain the auto-hide notice's 3s clear timer (#2639 follow-up)")
}

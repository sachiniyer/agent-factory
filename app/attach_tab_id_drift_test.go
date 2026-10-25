package app

import (
	"bytes"
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/session"
)

// TestAttachInstanceTab_DialsKeypressTabIDAfterRosterDrift is the regression
// guard for the ordinal-drift bug introduced by #1738: attachInstanceTab
// captured the tab as an ORDINAL at keypress but resolved the stable id only
// inside the deferred dial closure, after a window in which the background
// snapshot reconcile (ReconcileTabsFromData) can mutate the roster in place.
// The dial then bound whichever tab NOW occupies the stale ordinal — silently
// misrouting the user's keystrokes (including pasted, quoted shell commands) to
// a different tab of the correct session.
//
// The fix resolves and captures the tab's stable id eagerly at keypress — the
// synchronous event-loop step where the selection is provably current — exactly
// as captureSessionActionTarget already captures the session axis (#716/#717).
//
// This test reproduces the exact production window the report describes: a
// first-time attacher (the help overlay defers the dial past a help-dismiss
// keypress), an out-of-band close of the pressed-on tab via
// ReconcileTabsFromData (the snapshot path app/sync.go runs every 750ms), then
// the dismiss that fires the dial. It asserts the dial's tab_id is the
// keypress-time id of the pressed-on tab (now a dead id the daemon would reject
// with ErrTabGone) — NOT the id of the tab that shifted into the captured
// ordinal.
//
// On the buggy code (tabID resolved inside the deferred closure) this fails:
// inst.TabIDAt(1) at dial time returns shell-b's id (the shifted-in tab), so
// gotTabID == shellBID != shellAID.
func TestAttachInstanceTab_DialsKeypressTabIDAfterRosterDrift(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Do NOT mark helpTypeInstanceAttach seen: the first-time attach help overlay
	// is exactly what defers the dial past the help-dismiss keypress, opening the
	// keypress->dial window the bug exploits.

	// A genuinely STARTED local instance (worktree + mock tmux) is required: a
	// fake-backend row no-ops the reconcile, so only this harness reaches the real
	// ReconcileTabsFromData path that the snapshot drives. Start with the agent
	// tab plus two shell tabs so ordinal 1 (shell-a) is drift-exposed — the agent
	// tab at ordinal 0 is pinned by the reconcile (never dropped, never moved off
	// index 0), so only shell/process/web attaches (ordinal >=1) are vulnerable.
	inst := freshLocalInstance(t, "misroute") // agent tab only
	shellA, err := inst.AddShellTab()
	require.NoError(t, err)
	shellB, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 3, inst.TabCount(), "precondition: agent + shell-a + shell-b")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Capture the keypress-time ids — the same reads attachInstanceTab makes
	// under the fix. shell-a is at ordinal 1 (the attach target); shell-b at 2.
	shellAID, ok := inst.TabIDAt(1)
	require.True(t, ok)
	require.NotEmpty(t, shellAID, "precondition: shell-a must have a stable id")
	shellBID, ok := inst.TabIDAt(2)
	require.True(t, ok)
	require.NotEmpty(t, shellBID, "precondition: shell-b must have a stable id")
	require.Equal(t, shellA.ID, shellAID)
	require.Equal(t, shellB.ID, shellBID)
	require.NotEqual(t, shellAID, shellBID, "precondition: the two shells must have distinct ids")

	// Observe the WS PTY dial: capture the id the dial addresses the tab by (the
	// very argument the routing spy discarded) and detach immediately so the
	// post-detach lifecycle runs synchronously with no real daemon or PTY.
	var gotTabID string
	var gotTabIdx int
	var streamCalls int
	t.Cleanup(SetAttachStreamFnForTest(func(_ context.Context, _ apiclient.StreamSession, tabID string, tabIdx int) (chan struct{}, error) {
		streamCalls++
		gotTabID, gotTabIdx = tabID, tabIdx
		ch := make(chan struct{})
		close(ch)
		return ch, nil
	}))

	// Silence the post-detach terminal-mode reassert away from os.Stdout.
	swapRemoteDetachResetWriter(t, &bytes.Buffer{})

	// Keypress: attach on ordinal 1 (shell-a). The first-time help overlay shows
	// and defers the dial; attachTransitioning is NOT armed yet, and no cmd is
	// scheduled (showHelpScreen returns nil while the overlay is up).
	_, cmd := h.attachInstanceTab(inst, 1, "agent", "terminal")
	require.Nil(t, cmd, "the deferred dial must not schedule a cmd while the help overlay is up")
	require.Equal(t, stateHelp, h.state, "the first-time attach must park on the help overlay")
	require.NotNil(t, h.textOverlay, "the attach help overlay must be shown")
	require.False(t, h.attachTransitioning, "the attach is deferred to dismiss, not armed yet")

	// Out-of-band roster change while the overlay is open: another client (the
	// web UI, a second `af` process, `af sessions tab-*`) closed shell-a, and the
	// daemon's snapshot reconciles the local roster. Build the daemon's
	// authoritative tab list from the instance's own serialized Tabs, drop
	// shell-a, and reconcile exactly as handleSnapshot does (app/sync.go).
	data := inst.ToInstanceData()
	require.Len(t, data.Tabs, 3, "precondition: serialized roster has agent + shell-a + shell-b")
	target := []session.TabData{data.Tabs[0], data.Tabs[2]} // agent + shell-b (shell-a dropped)
	changed, err := inst.ReconcileTabsFromData(target)
	require.NoError(t, err)
	require.True(t, changed, "dropping shell-a out-of-band must report a roster change")

	// The drift: shell-b has shifted into ordinal 1. A dial-time
	// instance.TabIDAt(1) read now resolves shell-b's id — exactly the misroute.
	// This is the precondition that makes the assertion below meaningful.
	driftedID, ok := inst.TabIDAt(1)
	require.True(t, ok)
	require.Equal(t, shellBID, driftedID,
		"precondition: shell-b must have shifted into ordinal 1 after shell-a dropped")
	require.Len(t, inst.GetTabs(), 2, "precondition: shell-a is gone from the local roster")

	// Dismiss the help overlay with Enter (proceed with the attach — the
	// attachHelpDismissPolicy returns runOnDismiss=true for Enter). This
	// schedules the 20ms beginAttachTransition tick that ultimately runs the dial
	// through attachOverlayCallback -> attach() -> attachStreamFn.
	model, dismissCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	h = model.(*home)
	require.Equal(t, stateDefault, h.state, "Enter must dismiss the attach help overlay")
	require.NotNil(t, dismissCmd, "dismissing the overlay must schedule the deferred attach transition")

	// Drive the transition tick and the blocking attach callback to completion:
	// runAttachTransitionCmd drains the beginAttachMsg and runs the dial
	// synchronously inside h.Update, capturing the tabID via the spy above.
	_ = runAttachTransitionCmd(t, h, dismissCmd)
	endDetachWatchdog()

	require.Equal(t, 1, streamCalls, "the attach must dial the WS PTY stream exactly once")
	require.Equal(t, 1, gotTabIdx, "the dial must target the keypress-time ordinal")
	// THE FIX: the dial carries the stable id of the tab the user PRESSED on
	// (shell-a), captured at keypress time. Before the fix this read shell-b's
	// id (the shifted-in occupant) at dial time and the user's keystrokes silently
	// landed in the wrong tab.
	require.Equal(t, shellAID, gotTabID,
		"the dial must address the tab the user pressed on (its keypress-time id), "+
			"not the id of the tab that shifted into the stale ordinal after the roster drift")
	require.NotEqual(t, shellBID, gotTabID,
		"the dial must never silently bind to the shifted-in tab")
}

// TestAttachInstanceTab_DialsKeypressTabIDNoDrift is the happy-path companion
// (codex review class): with NO roster change between keypress and dial, the dial
// must address the tab's stable id, captured at keypress. Guards that the eager
// capture didn't regress the ordinary (overwhelmingly common) synchronous case
// AND closes the "the routing spy threw away the very tabID argument the fix
// asserts" gap the report flagged on TestAttachInstanceTab_RoutesEverySessionToStream.
func TestAttachInstanceTab_DialsKeypressTabIDNoDrift(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Skip the first-time attach help overlay so the deferred attach callback
	// runs synchronously inside attachInstanceTab (see showHelpScreen): no
	// keypress->dial window, no roster drift — the plain attach.
	require.NoError(t, h.appState.SetHelpScreensSeen(helpTypeInstanceAttach{}.mask()))

	inst := freshLocalInstance(t, "nodrift") // agent tab only
	shellA, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount(), "precondition: agent + shell-a")

	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	shellAID, ok := inst.TabIDAt(1)
	require.True(t, ok)
	require.NotEmpty(t, shellAID)
	require.Equal(t, shellA.ID, shellAID)

	var gotTabID string
	var gotTabIdx int
	var streamCalls int
	t.Cleanup(SetAttachStreamFnForTest(func(_ context.Context, _ apiclient.StreamSession, tabID string, tabIdx int) (chan struct{}, error) {
		streamCalls++
		gotTabID, gotTabIdx = tabID, tabIdx
		ch := make(chan struct{})
		close(ch)
		return ch, nil
	}))
	swapRemoteDetachResetWriter(t, &bytes.Buffer{})

	_, cmd := h.attachInstanceTab(inst, 1, "agent", "terminal")
	_ = runAttachTransitionCmd(t, h, cmd)
	endDetachWatchdog()

	require.Equal(t, 1, streamCalls, "the attach must dial the WS PTY stream exactly once")
	require.Equal(t, 1, gotTabIdx, "the dial must target the captured tab ordinal")
	require.Equal(t, shellAID, gotTabID,
		"the dial must address the pressed-on tab's stable id even in the no-drift case")
	// Sanity: the oracle id is unchanged (no drift happened) and the dial agrees.
	liveID, ok := inst.TabIDAt(1)
	require.True(t, ok)
	require.Equal(t, shellAID, liveID, "precondition: the roster did not drift")
	assert.Equal(t, liveID, gotTabID, "with no drift, the dial id equals the live id")
}

// TestAttachInstanceTab_AgentTabIDHealReachesDial is the regression guard for the
// agent-tab ID-heal interaction the eager capture opened: ReconcileTabsFromData
// replaces the agent tab's id even over a NON-EMPTY local id (the agentRow branch
// in session/instance_tab_reconcile.go), because the agent row is never dropped or
// re-added so a diverged id (a legacy restore + daemon restart minted different
// ids) can only be corrected by overwriting it. Capturing that id at keypress time
// freezes the stale pre-heal id; a first-time attach parked on the help overlay
// during the heal would then dial the stale id and the daemon would refuse it with
// ErrTabGone — a regression of the heal the reconcile deliberately performs.
//
// The agent tab is pinned at index 0 (the reconcile never drops or moves it), so
// re-resolving it at dial time cannot drift-bind a different tab — it only picks
// up the in-place heal. This test drives the heal through the production snapshot
// path (updateInstanceFromSnapshot) while the help overlay is open and asserts the
// dial addresses the HEALED id, not the stale keypress-time id.
func TestAttachInstanceTab_AgentTabIDHealReachesDial(t *testing.T) {
	resetDetachWatchdog(t)

	h := newTestHome(t)
	// Do NOT mark helpTypeInstanceAttach seen: the first-time attach help overlay
	// is the keypress->dial window the heal races.
	inst := freshLocalInstance(t, "heal") // agent tab only, at index 0
	require.Equal(t, 1, inst.TabCount(), "precondition: agent tab only")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	// Capture the agent tab's keypress-time id — the read attachInstanceTab makes
	// under the fix.
	pressAgentID, ok := inst.TabIDAt(0)
	require.True(t, ok)
	require.NotEmpty(t, pressAgentID, "precondition: the agent tab has a locally-minted id")

	// Observe the WS PTY dial: capture the id the dial addresses the tab by.
	var gotTabID string
	var gotTabIdx int
	var streamCalls int
	t.Cleanup(SetAttachStreamFnForTest(func(_ context.Context, _ apiclient.StreamSession, tabID string, tabIdx int) (chan struct{}, error) {
		streamCalls++
		gotTabID, gotTabIdx = tabID, tabIdx
		ch := make(chan struct{})
		close(ch)
		return ch, nil
	}))
	swapRemoteDetachResetWriter(t, &bytes.Buffer{})

	// Keypress: attach on ordinal 0 (the agent tab). The first-time help overlay
	// shows and defers the dial.
	_, cmd := h.attachInstanceTab(inst, 0, "agent", "terminal")
	require.Nil(t, cmd, "the deferred dial must not schedule a cmd while the help overlay is up")
	require.Equal(t, stateHelp, h.state, "the first-time attach must park on the help overlay")

	// While the overlay is open, the daemon's snapshot reconciles the local roster
	// and HEALS the agent tab's id — the ordinary legacy/restart divergence, not
	// an exotic one (mirrors TestSnapshot_AgentIDHealKeepsPaneOpen). The agent row
	// is never dropped or re-added, so the reconcile overwrites its id in place.
	staleAgentID := inst.GetTabs()[0].ID
	require.Equal(t, pressAgentID, staleAgentID)
	data := inst.ToInstanceData()
	data.Tabs[0].ID = "daemon-agent-id"
	h.updateInstanceFromSnapshot(inst, data)
	healedID, ok := inst.TabIDAt(0)
	require.True(t, ok)
	require.Equal(t, "daemon-agent-id", healedID,
		"precondition: the agent tab healed its id to the daemon's authoritative id")
	require.NotEqual(t, staleAgentID, healedID,
		"precondition: the heal changed the id — the keypress-time id is now stale")

	// Dismiss the help overlay with Enter (proceed with the attach). This schedules
	// the deferred dial.
	model, dismissCmd := h.handleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	h = model.(*home)
	require.Equal(t, stateDefault, h.state, "Enter must dismiss the attach help overlay")
	require.NotNil(t, dismissCmd, "dismissing the overlay must schedule the deferred attach transition")

	// Drive the transition tick and the blocking attach callback to completion.
	_ = runAttachTransitionCmd(t, h, dismissCmd)
	endDetachWatchdog()

	require.Equal(t, 1, streamCalls, "the attach must dial the WS PTY stream exactly once")
	require.Equal(t, 0, gotTabIdx, "the dial must target the agent tab at index 0")
	// THE FIX: the dial carries the HEALED id, not the stale keypress-time id the
	// eager capture froze. Before the heal-aware re-resolution the dial read the
	// stale id and the daemon refused it with ErrTabGone.
	require.Equal(t, "daemon-agent-id", gotTabID,
		"the dial must address the agent tab by its healed id, not the stale keypress-time id")
	require.NotEqual(t, staleAgentID, gotTabID,
		"the dial must not address the stale pre-heal id the daemon cannot resolve")
}

package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// Notice-clobber regression coverage for the account-login takeover.
//
// The TUI's status-bar notice system (errBox + the transientNoticeID
// generation token) hosts two kinds of notices that share one bar:
//   - deliberately-persistent spawn notices ("Starting the codex login for
//     "work"…") raised by handleAccountLogin with NO auto-clear, designed to
//     stand until the spawn reports back and retracts them under a generation
//     guard (if msg.noticeID == m.transientNoticeID) in
//     handleAccountLoginStarted;
//   - transient auto-hide notices ("X hidden — too narrow; resize…") raised
//     by setPaneAutoHideStatus when relayout can't fit every open pane.
//
// Before the fix, setPaneAutoHideStatus was gated only on
// configAgentSpawning — the config-agent in-flight guard (#4955) — so the
// account-login flow (a structural twin of the config-agent spawn, raising the
// SAME kind of persistent notice through the SAME setTransientNotice path
// with the SAME generation-token retraction) had NO equivalent in-flight
// guard. A window resize during the login daemon round trip that crossed
// layout.MultiPaneMinWidth downward while >=2 panes were open would overwrite
// the persistent "Starting…" login notice and bump transientNoticeID past
// the login's captured noticeID, so the login's later retraction was
// generation-guarded into a silent skip. The fix gates
// setPaneAutoHideStatus on accountLoginInFlight so the auto-hide notice is
// suppressed for the duration of the login round trip; because the suppressed
// path never sets pendingPaneAutoHideStatus, consumePaneAutoHideStatus's
// existing `pending == ""` early-return skips the second raise as well, so
// neither path bumps the generation token.
//
// These tests drive the real login handler (handleAccountLogin), the real
// resize path (updateHandleWindowSizeEvent -> relayout ->
// setPaneAutoHideStatus -> consumePaneAutoHideStatus), and the real
// retraction (handleAccountLoginStarted -> enterAccountLogin). They fail on
// the unfixed tree (the auto-hide notice clobbers the login notice and the
// retraction is skipped) and pass once setPaneAutoHideStatus is gated on
// accountLoginInFlight.

// TestPane_AutoHideDoesNotClobberAccountLoginSpawnNotice is the core
// regression: during the login daemon round trip, a resize that auto-hides a
// pane must not replace the persistent "Starting the codex login…"
// notice, must not advance transientNoticeID past the login's captured
// noticeID, and must not arm a deferred auto-hide status. The login's
// retraction must then fire and clear the notice — the contract the clobber
// used to silently break.
func TestPane_AutoHideDoesNotClobberAccountLoginSpawnNotice(t *testing.T) {
	h := paneTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)
	stubAccountLoginAttach(t)

	// Two panes open at a width that fits both, so a downward resize crosses
	// layout.MultiPaneMinWidth and auto-hides one of them — the precondition
	// for setPaneAutoHideStatus to fire.
	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes(), "two panes are open")
	require.Len(t, h.visiblePanes, 2, "both are visible at the wide width")

	// Reset any notice the open path left behind so the assertions isolate the
	// login notice and the resize that follows.
	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	// Start the login: arms the in-flight guard and posts the persistent
	// "Starting…" notice with no auto-clear, exactly as handleAccountLogin
	// does. handleAccountLogin returns the spawn cmd (a tea.Cmd), NOT a
	// (model, cmd) pair — there is no hotkey for the login, the config
	// overlay's Accounts section drives it through handleAccountRequest.
	cmd := h.handleAccountLogin("codex", "work")
	require.NotNil(t, cmd, "the login must start the daemon round trip")
	startedMsg := cmd().(accountLoginStartedMsg)
	require.NoError(t, startedMsg.err)
	require.True(t, h.accountLoginInFlight,
		"the in-flight guard is armed for the daemon round trip")

	startingNoticeID := startedMsg.noticeID
	require.Equal(t, startingNoticeID, h.transientNoticeID,
		"the login's notice is the current generation")
	require.Contains(t, h.errBox.FullError(), "Starting the codex login",
		"the persistent notice is on the bar")

	// Resize narrow enough to auto-hide a pane WHILE the login is in flight.
	// This is the clobber trigger: relayout auto-hides the LRU pane and
	// setPaneAutoHideStatus would raise the auto-hide notice.
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1, "the narrow width hides one of the two panes")

	// The fix: the persistent login notice must still be the one on the bar,
	// and the generation token must not have advanced past it.
	assert.Contains(t, h.errBox.FullError(), "Starting the codex login",
		"a resize during the login wait must not clobber the persistent notice")
	assert.NotContains(t, h.errBox.FullError(), "hidden",
		"the auto-hide guidance must not replace the login notice during the wait")
	assert.Equal(t, startingNoticeID, h.transientNoticeID,
		"transientNoticeID must not advance past the login's notice, or the "+
			"generation-guarded retraction would be silently skipped")
	assert.Empty(t, h.pendingPaneAutoHideStatus,
		"the auto-hide status is not armed while the login notice stands")

	// A resize wandering back and forth across the threshold during the wait
	// must not accumulate a deferred auto-hide status either: every narrow
	// crossing is suppressed, so the login's notice and generation survive.
	resizeHome(h, layout.MultiPaneMinWidth, 24) // wide enough to show both again
	require.Len(t, h.visiblePanes, 2, "both panes are visible again at the wide width")
	resizeHome(h, layout.MultiPaneMinWidth-1, 24) // narrow again
	require.Len(t, h.visiblePanes, 1, "narrow re-hides one pane")
	assert.Contains(t, h.errBox.FullError(), "Starting the codex login",
		"the persistent notice survives repeated resizes during the wait")
	assert.Equal(t, startingNoticeID, h.transientNoticeID,
		"the generation token stays at the login's notice across repeated resizes")
	assert.Empty(t, h.pendingPaneAutoHideStatus,
		"no deferred auto-hide status accumulates across resize crossings")

	// The login reports back. Because transientNoticeID is still the login's
	// own generation, the retraction fires and clears the "Starting…" notice —
	// the contract that the clobber used to silently break.
	// (handleAccountLogin captured noticeID; handleAccountLoginStarted retracts
	// under the guard, then hands the terminal over via enterAccountLogin.)
	model, _ := h.handleAccountLoginStarted(startedMsg)
	require.NotNil(t, model)
	assert.False(t, h.accountLoginInFlight,
		"the in-flight guard clears when the login reports back")
	assert.Empty(t, h.errBox.FullError(),
		"the login retracts its own notice once it reports back — no clobber to defeat the retraction")
}

// TestPane_AutoHideNoticeResurfacesAfterLoginSettles pins the other half of
// the contract: the auto-hide guidance is only SUPPRESSED while an account
// login is in flight, not permanently disabled. Once the login reports back
// and accountLoginInFlight clears, a narrow resize that auto-hides a pane must
// raise the "X hidden — too narrow…" notice exactly as before — so the fix
// does not regress the auto-hide feature (#1557 and its suite).
func TestPane_AutoHideNoticeResurfacesAfterLoginSettles(t *testing.T) {
	h := paneTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{
		Agent: "codex", Name: "work", Program: "codex login",
		SessionName: "af_af-login-codex-work", SocketPath: "/tmp/s/default",
	}, nil)
	stubAccountLoginAttach(t)

	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	cmd := h.handleAccountLogin("codex", "work")
	require.NotNil(t, cmd)
	startedMsg := cmd().(accountLoginStartedMsg)

	// A narrow resize during the login wait is suppressed.
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Contains(t, h.errBox.FullError(), "Starting the codex login",
		"the persistent notice survives the resize during the login wait")
	assert.NotContains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice is suppressed during the login wait")

	// The login settles, retracting the persistent notice and clearing the
	// in-flight guard.
	model, _ := h.handleAccountLoginStarted(startedMsg)
	require.NotNil(t, model)
	assert.False(t, h.accountLoginInFlight, "the guard clears on settle")
	assert.Empty(t, h.errBox.FullError(), "the login notice is retracted on settle")

	// Grow back to a width that fits both panes, then shrink again — now that
	// no login is in flight, the auto-hide notice must fire normally.
	resizeHome(h, layout.MultiPaneMinWidth, 24)
	require.Len(t, h.visiblePanes, 2, "both panes are visible again at the wide width")
	h.errBox.Clear() // drop anything the grow-up left on the bar

	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1, "the narrow width re-hides a pane")
	assert.Contains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice resurfaces once no login is in flight — the guard is login-scoped, not permanent")
	assert.NotContains(t, h.errBox.FullError(), "Starting",
		"no stale login notice lingers after the login settled")
}

// TestPane_AutoHideNoticeSuppressedDuringFailedLoginResumes confirms the guard
// is cleared even when the daemon refuses the login (an error outcome), so a
// failed login does not permanently suppress the auto-hide guidance. The
// guard is cleared unconditionally in handleAccountLoginStarted before the
// outcome branches run, mirroring the configAgentSpawning clear in
// handleConfigAgentSpawned.
func TestPane_AutoHideNoticeSuppressedDuringFailedLoginResumes(t *testing.T) {
	h := paneTestHome(t)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, errors.New(
		"agent does not support multiple accounts: af cannot log in to \"amp\""))
	stubAccountLoginAttach(t)

	alpha := h.store.GetInstanceByTitle("alpha")
	beta := h.store.GetInstanceByTitle("beta")
	require.NotNil(t, alpha)
	require.NotNil(t, beta)
	_, _ = h.openOrFocusPane(alpha, 0)
	_, _ = h.openOrFocusPane(beta, 0)
	require.Equal(t, 2, h.store.NumOpenPanes())

	h.errBox.Clear()
	h.pendingPaneAutoHideStatus = ""
	h.paneAutoHideNoticeID = 0

	cmd := h.handleAccountLogin("codex", "work")
	require.NotNil(t, cmd)
	startedMsg := cmd().(accountLoginStartedMsg)
	require.Error(t, startedMsg.err, "precondition: the daemon refused the login")
	require.True(t, h.accountLoginInFlight, "the guard is armed while the login is in flight")

	// A narrow resize during the failed login wait is suppressed too — the
	// guard protects the notice for every outcome, not just success.
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1)
	assert.NotContains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice is suppressed during the failed login wait as well")

	// The login reports back with an error: the guard must clear
	// unconditionally (a refusal must not suppress layout guidance forever).
	model, _ := h.handleAccountLoginStarted(startedMsg)
	require.NotNil(t, model)
	assert.False(t, h.accountLoginInFlight,
		"the in-flight guard clears on a failed login — auto-hide must resume")

	// After the failed login settles, a narrow resize (the panes are already
	// narrow here, so grow-then-shrink to re-cross the threshold) raises the
	// auto-hide notice normally again.
	resizeHome(h, layout.MultiPaneMinWidth, 24)
	require.Len(t, h.visiblePanes, 2)
	h.errBox.Clear()
	resizeHome(h, layout.MultiPaneMinWidth-1, 24)
	require.Len(t, h.visiblePanes, 1)
	assert.Contains(t, h.errBox.FullError(), "hidden",
		"the auto-hide notice resurfaces after a failed login settled")
}

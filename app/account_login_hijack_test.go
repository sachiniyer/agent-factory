package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// The async account-login result must not yank the user out of an overlay they
// navigated into while the daemon round trip was in flight.
//
// The login keypress itself closes the config editor and drops the user to
// stateDefault (handle_overlay.go: the takeover needs a clean terminal). The
// spawn is asynchronous, so the user is free to navigate away before the
// result lands — e.g. press `n` to start naming a session (stateNew), which
// leaves a namingInstance row and a pendingPrompt on the model. Before the
// fix, the started handler's `finished` branch and the done handler both
// funneled through reopenConfigWithAccountStatus, which unconditionally called
// showConfigEditor and set m.state = stateConfigEditor — ripping the user out
// of the in-progress naming flow and orphaning that state.
//
// These tests drive the handlers from a NAVIGATED overlay state (stateNew,
// stateRenameTab), asserting the fix: the state is left alone, the
// in-progress work is preserved, and the outcome is surfaced as a transient
// notice rather than a forced overlay reopen. The stateDefault and
// stateConfigEditor reopen paths — the designed behavior the existing
// TestAccountLoginDoneReportsWithoutReapingThePane pins — are re-pinned here
// so the allow-list is documented alongside the guard.

// ptrStatusErrBox returns the notice currently retained on the bar so a test
// can read it without the bar being sized for rendering. The bar renders only
// once its SetSize has run; the retained notice is independent of that.
func statusNoticeText(h *home) string {
	text, _ := h.errBox.RetainedNotice()
	return text
}

func statusNoticeIsFailure(h *home) bool {
	_, isFailure := h.errBox.RetainedNotice()
	return isFailure
}

// A done handler that lands while the user is naming must leave the naming
// flow exactly where it was: state, namingInstance and pendingPrompt all
// preserved, with the outcome shown as a transient notice instead of a forced
// config-editor reopen.
func TestAccountLoginDoneLeavesInProgressNamingAlone(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(200, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	inst := startNaming(t, h, "deploy-fix")
	h.pendingPrompt = "investigate the cache stampede"

	model, _ := h.handleAccountLoginDone(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.Same(t, h, model)
	assert.Equal(t, stateNew, h.state,
		"login done must not hijack the in-progress naming flow into the config editor")
	require.Same(t, inst, h.namingInstance,
		"the naming instance the user was editing must stay live, not be orphaned")
	assert.Equal(t, "investigate the cache stampede", h.pendingPrompt,
		"the user's half-typed prompt must stay on the model")
	assert.Contains(t, statusNoticeText(h), "Back from the codex login",
		"the login outcome must be surfaced as a transient notice when the user moved on")
	assert.False(t, statusNoticeIsFailure(h),
		"a successful login takeover is informational, not a failure notice")
}

// The `finished before handover` branch of the started handler has the same
// defect: it must not attach, must not reopen the editor, and must preserve
// the in-progress naming state, surfacing the outcome as a transient notice.
func TestAccountLoginFinishedLeavesInProgressNamingAlone(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(200, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	attached := stubAccountLoginAttach(t)

	inst := startNaming(t, h, "deploy-fix")

	model, _ := h.handleAccountLoginStarted(accountLoginStartedMsg{
		agent: "codex", name: "work", finished: true, loggedIn: true,
	})
	require.Same(t, h, model)
	assert.Equal(t, stateNew, h.state,
		"a finished-before-handover login must not hijack the in-progress naming flow")
	require.Same(t, inst, h.namingInstance,
		"the naming instance must not be orphaned by the finished branch")
	assert.Equal(t, 0, *attached, "the finished branch must never attach")
	assert.Contains(t, statusNoticeText(h), "is logged in",
		"the finished-login outcome must be surfaced as a transient notice")
}

// A finished login that left NO credential is a failure: it must still leave
// the user's state alone, but the transient notice must be raised as a
// failure so it stands out, and it must name the way to retry.
func TestAccountLoginFinishedFailureSurfacesNoticeWhenUserMovedOn(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(400, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)
	stubAccountLoginAttach(t)

	startNaming(t, h, "deploy-fix")

	model, _ := h.handleAccountLoginStarted(accountLoginStartedMsg{
		agent: "codex", name: "work", finished: true, loggedIn: false,
	})
	require.Same(t, h, model)
	assert.Equal(t, stateNew, h.state,
		"a failed finished-login must not hijack the naming flow either")
	assert.True(t, statusNoticeIsFailure(h),
		"a login that ended without a credential must surface as a failure notice")
	assert.Contains(t, statusNoticeText(h), "registered but not logged in")
	assert.Contains(t, statusNoticeText(h), "af accounts login codex work",
		"the notice must still name the way to retry it on the daemon host")
}

// The public Update dispatcher adds no guard of its own, so the message must
// arrive at the handler and be handled there. Routing through Update confirms
// the fix holds at the dispatch layer, not only when the handler is called
// directly.
func TestAccountLoginDoneViaUpdateLeavesInProgressNamingAlone(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(200, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	inst := startNaming(t, h, "deploy-fix")

	model, _ := h.Update(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.Same(t, h, model)
	assert.Equal(t, stateNew, h.state,
		"the Update dispatcher must not hijack the in-progress naming flow")
	require.Same(t, inst, h.namingInstance)
	assert.Contains(t, statusNoticeText(h), "Back from the codex login")
}

// The guard covers every active-overlay state, not only naming. A rename-tab
// prompt (stateRenameTab) the user opened while waiting must survive the
// async result without being dropped into the config editor.
func TestAccountLoginDoneLeavesRenameTabPromptAlone(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(200, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	h.state = stateRenameTab

	model, _ := h.handleAccountLoginDone(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.Same(t, h, model)
	assert.Equal(t, stateRenameTab, h.state,
		"login done must not hijack an in-progress rename-tab prompt")
	assert.Contains(t, statusNoticeText(h), "Back from the codex login")
}

// The error path of the done handler (a takeover that could not take the
// terminal) never reopens the editor — it raises the error directly. Pinning
// that it ALSO leaves a navigated overlay alone documents that this branch is
// not a second route back into the forced reopen.
func TestAccountLoginDoneErrorPathLeavesNamingAlone(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	h.errBox.SetSize(200, 1)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	inst := startNaming(t, h, "deploy-fix")

	_, cmd := h.handleAccountLoginDone(accountLoginDoneMsg{
		agent: "codex", name: "work", err: errors.New("exit status 1"),
	})
	assert.NotNil(t, cmd, "a failed takeover must raise the error")
	assert.Equal(t, stateNew, h.state,
		"the failed-takeover path must not hijack the naming flow")
	require.Same(t, inst, h.namingInstance)
	assert.True(t, statusNoticeIsFailure(h),
		"the failed takeover must surface as a failure notice")
}

// The allow-list: a result that lands while the user is STILL in stateDefault
// (the state the login keypress left them in) must reopen the config editor
// onto fresh state and report the outcome in the Accounts section. This is
// the designed behavior TestAccountLoginDoneReportsWithoutReapingThePane pins;
// re-pinned here so the guard's stateDefault case is documented next to the
// fix.
func TestAccountLoginDoneStillReopensFromStateDefault(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	model, _ := h.handleAccountLoginDone(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.Same(t, h, model)
	assert.Equal(t, stateConfigEditor, h.state,
		"from stateDefault the login result reopens the config editor as designed")
	assert.True(t, h.configPane.HasFocus(),
		"the config editor must own focus after the reopen")
	assert.Contains(t, h.configPane.String(), "Back from the codex login",
		"the Accounts section must report the outcome when the user is still positioned to see it")
}

// A user who reopened the config editor while waiting (stateConfigEditor) is
// also still positioned to receive the result, so the handler reopens onto
// fresh state and sets the status — same as stateDefault, no hijack.
func TestAccountLoginDoneReopensFromStateConfigEditor(t *testing.T) {
	h := newTestHome(t)
	sizeConfigPane(h)
	stubAccountSeams(t, daemon.AccountLoginResponse{}, nil)

	model, cmd := h.showConfigEditor()
	require.Same(t, h, model)
	require.True(t, h.configPane.HasFocus(),
		"precondition: the config editor is open")
	_ = cmd
	require.Equal(t, stateConfigEditor, h.state, "precondition: the user reopened the editor")

	model, _ = h.handleAccountLoginDone(accountLoginDoneMsg{agent: "codex", name: "work"})
	require.Same(t, h, model)
	assert.Equal(t, stateConfigEditor, h.state,
		"from stateConfigEditor the login result reopens the editor, reporting the outcome")
	assert.Contains(t, h.configPane.String(), "Back from the codex login")
}

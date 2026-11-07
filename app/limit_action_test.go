package app

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// limitActionInstance builds a started, mock-backed instance blocked at a usage
// limit (#1146) for the manual-retry action tests.
func limitActionInstance(t *testing.T, title string, resetAt time.Time) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetLimitReached(resetAt)
	return inst
}

// TestHandleLimitRetry_NonLimitRow_NoDispatch: pressing c on a session that is
// not usage-limit-blocked surfaces a message and never fires the resume RPC.
func TestHandleLimitRetry_NonLimitRow_NoDispatch(t *testing.T) {
	h := newTestHome(t)
	info, errorLogs := captureHomeMessageLogs(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "test"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error { called = true; return nil })
	defer restore()

	_, _ = h.handleLimitRetry()
	require.False(t, called, "a non-limit row must not dispatch the resume RPC")
	require.Contains(t, info.String(), "session 'worker' is not blocked on a usage limit",
		"the truthful reason for declining the action remains visible at INFO")
	require.Empty(t, errorLogs.String(),
		"a designed action refusal must not be indistinguishable from an operation failure")
}

// TestHandleLimitRetry_LimitRow_Dispatches: pressing c on a limit-blocked row
// dispatches the resume command, which routes through the daemon seam.
func TestHandleLimitRetry_LimitRow_Dispatches(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.NotNil(t, cmd, "a limit row must dispatch a resume command")
	msg := cmd()
	done, ok := msg.(limitRetriedMsg)
	require.True(t, ok, "the command must emit limitRetriedMsg")
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest,
		"the resume command must preserve the selected session's stable identity")
}

func TestHandleLimitRetry_UnconfirmedAccountHandoffDispatches(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: true, Mission: "continue", From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus: session.PromptCouldNotConfirm,
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	// A confirmable row gives `c` two verbs (#4429): the resolve picker opens
	// instead of dispatching a bare resend.
	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.NotNil(t, h.selectionOverlay)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions)

	// Choosing "Retry send" dispatches the resume against the captured identity.
	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "an inspected, unconfirmed handoff must expose an explicit retry")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

func TestHandleLimitRetry_UnconfirmedAgentHandoffDispatches(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "gemini",
	})
	require.NoError(t, err)
	base.SetBackend(session.NewFakeBackend())
	base.SetStartedForTest(true)
	base.SetStatusForTest(session.Running)
	require.NoError(t, base.Transition(session.BeginHandoff()))
	mission := "continue the inherited work"
	base.SetPendingHandoffMission(mission)
	require.NoError(t, base.BeginPendingHandoffMissionDelivery(mission))
	require.NoError(t, base.RecordPendingHandoffMissionDelivery(mission, session.PromptCouldNotConfirm))
	h.store.AddInstance(base)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions)

	// The resend arm still dispatches the retry against the captured identity.
	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "an inspected, ambiguous agent handoff must expose the c retry action")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: base.ID, Title: base.Title, RepoID: h.repoID}, gotRequest)
}

// TestHandleLimitRetry_PickerMarkDeliveredDispatches covers the picker's second
// verb: choosing "Mark delivered" routes the confirm RPC against the captured
// identity — the no-resend exit (#4429).
func TestHandleLimitRetry_PickerMarkDeliveredDispatches(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "gemini",
	})
	require.NoError(t, err)
	base.SetBackend(session.NewFakeBackend())
	base.SetStartedForTest(true)
	base.SetStatusForTest(session.Running)
	require.NoError(t, base.Transition(session.BeginHandoff()))
	mission := "continue the inherited work"
	base.SetPendingHandoffMission(mission)
	require.NoError(t, base.BeginPendingHandoffMissionDelivery(mission))
	require.NoError(t, base.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	h.store.AddInstance(base)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ConfirmHandoffDeliveryRequest
	restore := SetHandoffDeliveryConfirmerForTest(func(request daemon.ConfirmHandoffDeliveryRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd)
	require.Equal(t, stateSelectHandoffResolve, h.state)

	// Move to "Mark delivered" and submit.
	h.selectionOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "the mark-delivered arm must dispatch the confirm")
	done, ok := cmd().(handoffDeliveryConfirmedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ConfirmHandoffDeliveryRequest{ID: base.ID, Title: base.Title, RepoID: h.repoID}, gotRequest)
}

// A row that is only usage-limited still opens the picker when its pending
// mission is confirmable, and its resume row runs the plain limit resume —
// which un-stalls the agent and does NOT resend the mission. Labelling that row
// "Retry send" promised a resend the dispatch never performs.
func TestHandleLimitRetry_LimitOnlyPickerDoesNotPromiseAResend(t *testing.T) {
	h := newTestHome(t)
	base := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	mission := "continue the inherited work"
	base.SetPendingHandoffMission(mission)
	require.NoError(t, base.RecordPendingHandoffMissionDelivery(mission, session.PromptSentUnverified))
	h.store.AddInstance(base)
	h.sidebar.SetSelectedInstance(0)
	require.True(t, base.LimitReached(), "fixture: the row is usage-limited")
	require.False(t, base.CanRetryPendingHandoffMissionDelivery(), "fixture: no mission resend is admitted")
	require.True(t, base.CanConfirmPendingHandoffDelivery(), "fixture: the mission is confirmable")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd)
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "the limit resume stays reachable from `c`")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Resume from limit")
	require.NotContains(t, rendered, "Retry send",
		"a plain limit resume must not be offered as a mission resend")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: base.ID, Title: base.Title, RepoID: h.repoID}, gotRequest)
}

// TestHandleLimitRetry_ManualSwapAtLimitOffersHonestRetrySendLabel is the
// manual-swap counterpart of TestHandleLimitRetry_LimitOnlyPickerDoesNotPromiseAResend.
// A manual account swap parked at its usage-limit wall with an ambiguous
// mission verdict is NOT the agent-handoff twin: the daemon's ResumeFromLimit
// fork re-pastes the pending mission (deliverManualAccountMission), so the
// row's restart IS a resend. The picker must label Row 0 "Retry send" — the
// honest resend — rather than the agent-handoff-twin's "Resume from limit —
// the pending mission is not resent", which is true only for the arm whose
// daemon sends the session goal (SendPromptWithEvidence) without resending.
func TestHandleLimitRetry_ManualSwapAtLimitOffersHonestRetrySendLabel(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveLimitReached
	data.LimitResetAt = time.Now().Add(time.Hour)
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: true, Mission: "continue", From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus: session.PromptCouldNotConfirm,
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.True(t, inst.LimitReached(), "fixture: the row is parked at its usage-limit wall")
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"fixture: the mission verdict is ambiguous")
	require.True(t, inst.CanRetryPendingManualAccountSwapDelivery(),
		"the fix: the limit wall admits the explicit retry because the manual-swap resume re-sends the mission")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.NotNil(t, h.selectionOverlay)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume/resend and Row 1 is the confirm")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Retry send",
		"the manual-swap resume re-sends the pending mission, so Row 0 must be labelled honestly as a resend")
	require.NotContains(t, rendered, "Resume from limit",
		"the agent-handoff-twin's no-resend label must not appear on a row whose daemon resends")
	require.NotContains(t, rendered, "not resent",
		"the false \"pending mission is not resent\" promise must not appear for a row whose daemon resends")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the resend) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// TestHandleLimitRetry_AutomaticSwapAtLimitOffersHonestRetrySendLabel is the
// #6800 regression: an AUTOMATIC account swap parked at its usage-limit wall
// with an ambiguous notice verdict routes through the same ResumeFromLimit
// fork whose daemon re-submits the committed notice
// (resumeFromLimitLockedOutcome → deliverAutomaticAccountNotice →
// SendPromptWithEvidence). The row's restart IS a resend — but on master the
// retry classification only admits manual swaps and agent handoffs, so Row 0
// falls to "Resume from limit — the pending mission is not resent", a false
// promise. The picker must instead label Row 0 "Retry send" — the honest
// resend — exactly like the manual-swap arm.
func TestHandleLimitRetry_AutomaticSwapAtLimitOffersHonestRetrySendLabel(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveLimitReached
	data.LimitResetAt = time.Now().Add(time.Hour)
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: false, From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus:    session.PromptCouldNotConfirm,
		MissionDeliveryAttemptAt: time.Now().Add(-time.Minute),
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.True(t, inst.LimitReached(), "fixture: the row is parked at its usage-limit wall")
	require.True(t, inst.PendingAccountSwapDeliveryUnconfirmed(),
		"fixture: the notice verdict is ambiguous")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.NotNil(t, h.selectionOverlay)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume/resend and Row 1 is the confirm")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Retry send",
		"the automatic swap's resume re-submits the committed notice — Row 0 must be labelled honestly as a resend")
	require.NotContains(t, rendered, "Resume from limit",
		"the no-resend label must not appear on a row whose daemon resends the notice")
	require.NotContains(t, rendered, "not resent",
		"the false \"pending mission is not resent\" promise must not appear for a row whose daemon resends")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the resend) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// A committed swap parked at the wall on a startup-unknown row is still
// resent by the daemon: the committed-transaction bypass in ResumeFromLimit
// (committedAccountSwap) admits it, and BeginManualAccountSwap /
// BeginLimitResume skip the startup gate for the committed swap because the
// explicit retry IS the inspection the marker waits for. So the picker must
// say "Retry send" here too — "the pending mission is not resent" would be
// the same false promise as #6800's (MED, #6922 review).
func TestHandleLimitRetry_StartupUnknownManualSwapAtLimitOffersHonestRetrySendLabel(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveLimitReached
	data.LimitResetAt = time.Now().Add(time.Hour)
	data.StartupStateUnknown = true
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: true, Mission: "continue", From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus: session.PromptCouldNotConfirm,
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.True(t, inst.LimitReached(), "fixture: the row is parked at its usage-limit wall")
	require.True(t, inst.StartupStateUnknown(), "fixture: the replacement's startup went blind")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume/resend and Row 1 is the confirm")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Retry send",
		"the daemon resends the committed manual mission through the startup gate bypass — Row 0 must not promise otherwise")
	require.NotContains(t, rendered, "not resent")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the resend) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// The automatic twin of the startup-unknown arm: a committed automatic swap
// parked at the wall resends its notice through the same committed-swap
// bypass (resumeFromLimitLockedOutcome → deliverAutomaticAccountNotice), so
// startup-unknown must not demote the row to the "not resent" label.
func TestHandleLimitRetry_StartupUnknownAutomaticSwapAtLimitOffersHonestRetrySendLabel(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveLimitReached
	data.LimitResetAt = time.Now().Add(time.Hour)
	data.StartupStateUnknown = true
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: false, From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus:    session.PromptCouldNotConfirm,
		MissionDeliveryAttemptAt: time.Now().Add(-time.Minute),
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.True(t, inst.LimitReached(), "fixture: the row is parked at its usage-limit wall")
	require.True(t, inst.StartupStateUnknown(), "fixture: the replacement's startup went blind")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume/resend and Row 1 is the confirm")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Retry send",
		"the daemon resends the committed automatic notice through the startup gate bypass — Row 0 must not promise otherwise")
	require.NotContains(t, rendered, "not resent")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the resend) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// An automatic swap whose row is NOT parked at the wall is a daemon no-op:
// resumeFromLimitLockedOutcome returns unperformed for a non-limited
// automatic swap, so the picker must not offer a resend the daemon will not
// perform — the confirm row stands alone (LOW, #6922 review).
func TestHandleLimitRetry_NonLimitAutomaticSwapShowsNoResendRow(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: false, From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus:    session.PromptCouldNotConfirm,
		MissionDeliveryAttemptAt: time.Now().Add(-time.Minute),
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.False(t, inst.LimitReached(), "fixture: the row is not parked at a usage-limit wall")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotConfirm daemon.ConfirmHandoffDeliveryRequest
	restoreConfirm := SetHandoffDeliveryConfirmerForTest(func(request daemon.ConfirmHandoffDeliveryRequest) error {
		gotConfirm = request
		return nil
	})
	defer restoreConfirm()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveConfirm}, h.handoffResolve.actions,
		"the daemon does not resend a non-limited automatic swap — the picker must not offer a resend row")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.NotContains(t, rendered, "Retry send")
	require.NotContains(t, rendered, "Resume from limit")
	require.Contains(t, rendered, "Mark delivered")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	done, ok := cmd().(handoffDeliveryConfirmedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ConfirmHandoffDeliveryRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotConfirm)
}

// A startup-unknown manual swap OFF its limit wall is still an honest
// resend: the committed-swap bypass admits the explicit retry and
// BeginManualAccountSwap skips the startup check, so the daemon re-pastes
// the pending mission even though the row is not parked — the "Retry send"
// label stays truthful (#6800 follow-up).
func TestHandleLimitRetry_StartupUnknownManualSwapOffLimitOffersHonestRetrySendLabel(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveRunning
	data.StartupStateUnknown = true
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	data.Account = "personal"
	data.PendingAccountSwap = &session.AccountSwapData{
		Manual: true, Mission: "continue", From: "work", To: "personal", ReplacementPanesStarted: true,
		MissionDeliveryStatus: session.PromptCouldNotConfirm,
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.False(t, inst.LimitReached(), "fixture: the row is not parked at a usage-limit wall")
	require.True(t, inst.StartupStateUnknown(), "fixture: the replacement's startup went blind")
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume/resend and Row 1 is the confirm")
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.Contains(t, rendered, "Retry send",
		"the daemon resends the committed manual mission past the startup gate — Row 0 must say so")
	require.NotContains(t, rendered, "not resent")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the resend) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// An agent handoff parked at its limit on a startup-unknown row is NOT a
// resend: retryPendingHandoff's LiveLimitReached arm transfers the mission
// into the scheduled limit retry (SetPrompt + clear + persist) and returns
// unperformed without submitting. Row 0 must therefore carry the truthful
// no-resend label — "Resume from limit — the pending mission is not
// resent" — not "Retry send" (#6800 follow-up).
func TestHandleLimitRetry_StartupUnknownAgentHandoffAtLimitLabelsResumeNotResend(t *testing.T) {
	h := newTestHome(t)
	base, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "gemini",
	})
	require.NoError(t, err)
	data := base.ToInstanceData()
	data.Status = session.Running
	data.Liveness = session.LiveLimitReached
	data.LimitResetAt = time.Now().Add(time.Hour)
	data.StartupStateUnknown = true
	data.PendingHandoffMission = "continue the inherited work"
	data.HandoffDeliveryStatus = session.PromptCouldNotConfirm
	data.Worktree = session.GitWorktreeData{
		RepoPath: data.Path, WorktreePath: data.Path, SessionName: data.Title, ExternalWorktree: true,
	}
	inst, err := session.FromInstanceData(data)
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	require.True(t, inst.LimitReached(), "fixture: the row is parked at its usage-limit wall")
	require.True(t, inst.StartupStateUnknown(), "fixture: the replacement's startup went blind")
	require.True(t, inst.CanConfirmPendingHandoffDelivery(),
		"fixture: the row is confirmable, so the resolve picker opens")

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.Nil(t, cmd, "a confirmable row opens the resolve picker rather than dispatching")
	require.Equal(t, stateSelectHandoffResolve, h.state)
	require.Equal(t, []handoffResolveAction{handoffResolveResend, handoffResolveConfirm},
		h.handoffResolve.actions, "Row 0 is the resume and Row 1 is the confirm")
	h.selectionOverlay.SetWidth(140) // the default test width truncates the label
	rendered := ansi.Strip(h.selectionOverlay.Render())
	require.NotContains(t, rendered, "Retry send",
		"the daemon's limit arm transfers the mission to scheduled retry without submitting — Row 0 must not promise a resend")
	require.Contains(t, rendered, "Resume from limit",
		"the truthful label for a parked handoff whose resume transfers rather than resends")
	require.Contains(t, rendered, "not resent")

	_, cmd = h.handleStateSelectHandoffResolve(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd, "Row 0 dispatches the resume (the transfer) against the captured identity")
	done, ok := cmd().(limitRetriedMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, daemon.ResumeFromLimitRequest{ID: inst.ID, Title: inst.Title, RepoID: h.repoID}, gotRequest)
}

// A manual retry retains its target while the tea.Cmd waits to run. If a
// snapshot replaces the selected row with a different session that reused the
// title in that window, the pending retry must not deliver the old prompt into
// the replacement.
func TestHandleLimitRetry_DoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	var gotRequest daemon.ResumeFromLimitRequest
	restore := SetLimitResumerForTest(func(request daemon.ResumeFromLimitRequest) error {
		gotRequest = request
		return nil
	})
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.NotNil(t, cmd)

	replacement := limitActionInstance(t, original.Title, time.Now().Add(2*time.Hour))
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_ = cmd()
	require.Equal(t, original.ID, gotRequest.ID,
		"the pending retry must retain the original session's stable ID")
	require.NotEqual(t, replacement.ID, gotRequest.ID,
		"a same-title replacement must not inherit the pending retry")
}

// TestHandleLimitRetry_TearingDownRow_NoDispatch: pressing c on a limit-blocked
// row that is already being deleted must not race a resume RPC against teardown.
func TestHandleLimitRetry_TearingDownRow_NoDispatch(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	inst.SetInFlightOpForTest(session.OpKilling)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	called := false
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error { called = true; return nil })
	defer restore()

	_, cmd := h.handleLimitRetry()
	require.False(t, called, "a deleting row must not dispatch the resume RPC")
	require.NotNil(t, cmd, "the user should get a transient error message")
	require.Contains(t, h.errBox.String(), "session 'worker' is being deleted")
}

// TestResumeFromLimitCmd_SurfacesError: a daemon rejection is carried back on the
// completion message (handled into the error box, limit state left intact).
func TestResumeFromLimitCmd_SurfacesError(t *testing.T) {
	h := newTestHome(t)
	restore := SetLimitResumerForTest(func(daemon.ResumeFromLimitRequest) error {
		return errors.New("session is not blocked on a usage limit")
	})
	defer restore()

	target := sessionActionTarget{id: "worker-id", title: "worker", repoID: h.repoID}
	msg := h.resumeFromLimitCmd(target)()
	done, ok := msg.(limitRetriedMsg)
	require.True(t, ok)
	require.Error(t, done.err)
}

// TestHandleLimitRetried_ClearsLocally: on a successful retry the local row's
// limit state is cleared immediately, without waiting for the next snapshot.
func TestHandleLimitRetried_ClearsLocally(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	require.True(t, inst.LimitReached())

	target := captureSessionActionTarget(inst, h.repoID)
	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target})
	require.False(t, inst.LimitReached(), "a successful retry must clear the local limit state")
}

func TestHandleLimitRetried_DoesNotClearSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(original)
	target := captureSessionActionTarget(original, h.repoID)

	replacement := limitActionInstance(t, original.Title, time.Now().Add(2*time.Hour))
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target})
	require.True(t, replacement.LimitReached(),
		"the old retry completion must not clear a same-title replacement's limit state")
}

func TestHandleLimitRetried_NoOpKeepsLimitLocally(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	target := captureSessionActionTarget(inst, h.repoID)

	_, _ = h.handleLimitRetried(limitRetriedMsg{target: target, err: errors.New("resume was not performed: another operation owns the retry")})
	require.True(t, inst.LimitReached(), "a daemon no-op must not clear the local limit state")
}

func TestHandleLimitRetried_CommittedWarningClearsLocallyAndWarns(t *testing.T) {
	h := newTestHome(t)
	inst := limitActionInstance(t, "worker", time.Now().Add(time.Hour))
	h.store.AddInstance(inst)
	target := captureSessionActionTarget(inst, h.repoID)

	_, _ = h.handleLimitRetried(limitRetriedMsg{
		target: target,
		err:    committedHandoffNoticeError{},
	})
	require.False(t, inst.LimitReached(), "a committed retry already landed and must clear local limit state")
	require.Contains(t, h.errBox.FullError(), "settlement is pending")
}

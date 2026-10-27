package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// withRemoteConfirmProbeTimeout shrinks the OPERATOR-initiated probe budget
// (remoteConfirmProbeTimeout) for one test and restores it after. The poll
// loop's remoteLostConfirmTimeout is a separate knob (see withRemoteLossThresholds)
// since cc0208b0's bug was exactly that the operator verbs reused the poll's
// budget — these tests pin the split.
func withRemoteConfirmProbeTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	prev := remoteConfirmProbeTimeout
	remoteConfirmProbeTimeout = timeout
	t.Cleanup(func() { remoteConfirmProbeTimeout = prev })
}

// delayedAliveRemoteServer is an httptest agent-server for a COLD-BUT-LIVE
// remote: /v1/agent/alive eventually answers {"alive": true} after `delay`. The
// answer is slow, not absent, so the only thing that can refuse the operator
// verbs is the probe BUDGET running out before the answer lands — the exact line
// cc0208b0's reuse of the poll's 5s budget cut. /v1/agent/snapshot serves a fast
// idle capture so a poll-thread caller reaches its own idle-branch probe rather
// than the debounce. The alive wait is interruptible by the request context so a
// probe that has already given up (or a test closing the server) does not stall
// cleanup on the remainder of the delay.
func delayedAliveRemoteServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agent/snapshot":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"updated": false, "has_prompt": false, "content": ""},
			})
		case "/v1/agent/alive":
			if delay > 0 {
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alive": true}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stageRemoteHandoffWedge registers a remote-workspace instance in the exact
// wedge shape #4429 reported — OpReplacing, startup_state_unknown, a pending
// ambiguous mission — and points its agent-server at url. The operator exits
// (confirm / explicit retry) probe /v1/agent/alive over real HTTP, exactly as
// they do against a docker/ssh session in production.
func stageRemoteHandoffWedge(t *testing.T, m *Manager, repoID, repoPath, title, url string, status session.PromptDeliveryStatus) *session.Instance {
	t.Helper()
	inst, _ := registerStartedRemote(t, m, repoID, repoPath, title, url, session.Running)
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	require.NoError(t, inst.RecordPendingHandoffMissionDelivery(mission, status))
	inst.MarkStartupStateUnknown()
	require.NoError(t, inst.Transition(session.BeginHandoff()))
	return inst
}

// TestConfirmHandoffDelivery_RemoteProbeTimeoutRefusesLiveButSlowRuntime is the
// safety half of the operator-confirm exit on a remote: a runtime whose
// /v1/agent/alive cannot answer within the operator probe budget is NOT
// confirmed — an unreachable runtime cannot have its delivery attested, and
// probeUnknown keeps the row wedged for restore/kill rather than guessing.
//
// The remote here is LIVE-but-slow: the alive handler eventually answers
// alive=true, just past the (shrunk) operator budget. Before the fix, the
// confirm verb reused the poll loop's 5s remoteLostConfirmTimeout, so a cold
// link whose dial alone exceeds 5s refused a live runtime every retry; now the
// confirm verb reads its OWN budget (remoteConfirmProbeTimeout), so shrinking
// THAT budget is what makes this remote refuse. (Without the fix this test
// fails: the confirm would read the unshrunk 5s poll budget, the 200ms answer
// would land well inside it, and the confirm would SUCCEED instead of refusing.)
func TestConfirmHandoffDelivery_RemoteProbeTimeoutRefusesLiveButSlowRuntime(t *testing.T) {
	withRemoteConfirmProbeTimeout(t, 50*time.Millisecond)
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := delayedAliveRemoteServer(t, 200*time.Millisecond)
	inst := stageRemoteHandoffWedge(t, manager, repoID, repoPath, "wedged-remote-slow", srv.URL, session.PromptSentUnverified)
	require.True(t, inst.CanConfirmPendingHandoffDelivery(), "fixture: the wedge advertises its supported exit")
	mission := inst.PendingHandoffMission()

	start := time.Now()
	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	elapsed := time.Since(start)

	require.Error(t, err)
	require.False(t, performed)
	require.ErrorContains(t, err, "could not be confirmed live")
	require.ErrorContains(t, err, "the liveness probe got no answer",
		"the refusal must name the probeUnknown verdict in an operator's words")
	require.Less(t, elapsed, 2*time.Second,
		"the refusal must come near the probe budget, not the 30s transport call timeout — the operator budget bounds the wait")

	// The row stays exactly wedged: restore/kill still own it.
	require.Equal(t, mission, inst.PendingHandoffMission(),
		"a refused confirm leaves the obligation for restore/kill")
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp(), "the replacement fence stays up")
	require.True(t, inst.StartupStateUnknown(), "the startup-unknown marker stays")
	require.False(t, inst.Started(), "a refused confirm must not restore a binding nothing proved")
}

// TestConfirmHandoffDelivery_RemoteProbeFastAnswerSucceeds is the control for the
// refusal above: the SAME remote, the SAME shrunk operator budget, but the alive
// answer lands UNDER the budget. The confirm succeeds and retires the mission —
// proving the budget line, not the remote, decided the refusal. This is the
// exact contrast the bug report used to isolate the 5s budget as the cause.
func TestConfirmHandoffDelivery_RemoteProbeFastAnswerSucceeds(t *testing.T) {
	withRemoteConfirmProbeTimeout(t, 50*time.Millisecond)
	manager, repoID, repoPath := newStatusTestManager(t)
	srv := delayedAliveRemoteServer(t, 10*time.Millisecond) // under the 50ms budget
	inst := stageRemoteHandoffWedge(t, manager, repoID, repoPath, "wedged-remote-fast", srv.URL, session.PromptSentUnverified)
	require.True(t, inst.CanConfirmPendingHandoffDelivery())

	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.True(t, performed)

	require.Empty(t, inst.PendingHandoffMission(), "a fast live answer retires the obligation")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the replacement fence settles")
	require.False(t, inst.StartupStateUnknown(), "the probe-backed attestation resolves the flag")
	require.True(t, inst.Started(), "the probe restores the binding the flag lowered")

	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Empty(t, rec.PendingHandoffMission,
		"the retired obligation must be durable, or a restart would resurrect the wedge")
}

// TestConfirmHandoffDelivery_RemoteProbeUsesOperatorBudgetNotPollBudget is THE
// FIX for cc0208b0's budget reuse: the operator confirm probe must wait on its
// OWN budget (remoteConfirmProbeTimeout), not the poll loop's 5s tie-break
// budget (remoteLostConfirmTimeout).
//
// The alive delay (200ms) is chosen BETWEEN the two budgets: it is slower than
// the poll's shrunk 50ms so a probe that wrongly reused the poll budget would
// time out and the confirm would REFUSE, but faster than the operator's 500ms
// so the operator-budget probe answers and the confirm SUCCEEDS. Without the
// fix — confirm reusing remoteLostConfirmTimeout (50ms) — this remote times out
// and the confirm refuses; with the fix, the operator-budget probe answers and
// the confirm retires the mission. The only difference between pass and fail is
// WHICH budget the probe reads.
func TestConfirmHandoffDelivery_RemoteProbeUsesOperatorBudgetNotPollBudget(t *testing.T) {
	// Poll loop's tie-break budget, shrunk for the test. A confirm that wrongly
	// reused this would time out at 50ms against a 200ms answer.
	withRemoteLossThresholds(t, 3, time.Minute, 50*time.Millisecond)
	// The operator's own budget, sized ABOVE the 200ms answer so the one-shot
	// RPC probe outlives the cold-but-live remote.
	withRemoteConfirmProbeTimeout(t, 500*time.Millisecond)

	manager, repoID, repoPath := newStatusTestManager(t)
	srv := delayedAliveRemoteServer(t, 200*time.Millisecond)
	inst := stageRemoteHandoffWedge(t, manager, repoID, repoPath, "wedged-remote-budget-split", srv.URL, session.PromptSentUnverified)
	require.True(t, inst.CanConfirmPendingHandoffDelivery())

	performed, err := manager.confirmHandoffDelivery(ConfirmHandoffDeliveryRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	require.NoError(t, err)
	require.True(t, performed,
		"the operator budget must outlive a cold-but-live remote that the poll budget would have cut off — "+
			"the confirm probe reads remoteConfirmProbeTimeout, not remoteLostConfirmTimeout")

	require.Empty(t, inst.PendingHandoffMission(), "confirm retires the mission once the probe answers")
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.False(t, inst.StartupStateUnknown())

	rec := recordFor(t, repoID, inst.Title)
	require.NotNil(t, rec)
	require.Empty(t, rec.PendingHandoffMission)
}

// TestRetryHandoffDelivery_RemoteProbeTimeoutRefusesLiveButSlowRuntime is the
// retry path's twin of the confirm refusal: the startup-unknown arm of
// retryPendingHandoff probes the pane before resending, and a probe that
// cannot answer within the operator budget refuses and leaves the row wedged
// for restore/kill — never spending the readiness timeout under the session
// locks and never resending into a runtime it could not prove live.
//
// Like the confirm, the retry now reads remoteConfirmProbeTimeout (the operator
// budget), so shrinking THAT budget is what makes this live-but-slow remote
// refuse. The budget-split itself is pinned by
// TestConfirmHandoffDelivery_RemoteProbeUsesOperatorBudgetNotPollBudget, which
// exercises the shared probeLivenessForOperator helper this retry path also
// uses; this test pins the refusal behavior of the retry path specifically.
func TestRetryHandoffDelivery_RemoteProbeTimeoutRefusesLiveButSlowRuntime(t *testing.T) {
	// Shrink both budgets so the refusal is fast regardless of which budget a
	// probe reads; this isolates the retry's probeUnknown-refusal behavior.
	// (The confirm test above proves the operator probe reads the operator
	// budget specifically, by setting the two budgets to different values.)
	withRemoteLossThresholds(t, 3, time.Minute, 50*time.Millisecond)
	withRemoteConfirmProbeTimeout(t, 50*time.Millisecond)

	manager, repoID, repoPath := newStatusTestManager(t)
	srv := delayedAliveRemoteServer(t, 200*time.Millisecond)
	inst := stageRemoteHandoffWedge(t, manager, repoID, repoPath, "wedged-remote-retry-slow", srv.URL, session.PromptSentUnverified)
	require.True(t, inst.CanRetryPendingHandoffMissionDelivery(),
		"fixture: the wedge advertises the explicit retry exit")
	mission := inst.PendingHandoffMission()

	start := time.Now()
	outcome, err := manager.resumeFromLimitOutcome(ResumeFromLimitRequest{
		ID: inst.ID, Title: inst.Title, RepoID: repoID,
	})
	elapsed := time.Since(start)

	require.ErrorContains(t, err, "could not be confirmed live")
	require.ErrorContains(t, err, "the liveness probe got no answer",
		"the retry refusal must name the probeUnknown verdict in words")
	require.Equal(t, resumeNotPerformed, outcome)
	require.Less(t, elapsed, 2*time.Second,
		"the refusal must come near the probe budget, not the readiness or transport timeout")

	// The row stays exactly as the wedge left it.
	require.Equal(t, mission, inst.PendingHandoffMission(),
		"a refused retry leaves the obligation for restore/kill")
	require.Equal(t, session.OpReplacing, inst.GetInFlightOp())
	require.True(t, inst.StartupStateUnknown(), "the unknown marker stays; nothing was proved")
	require.False(t, inst.Started(), "a refused retry must not restore a binding nothing proved")
}

// TestRefreshStatuses_PollIdleProbeKeepsShortPollBudget pins the SCOPE of the
// fix: the poll loop's idle-branch probe (refreshInstanceStatus → probeLiveness)
// must keep reading the SERIAL-WALK tie-break budget (remoteLostConfirmTimeout),
// NOT the new operator budget. The 5s budget's justification — a serial
// RefreshStatuses walk that one slow probe could stall for every other session
// (#1794) — applies to this caller unchanged, so splitting the budgets must not
// loosen it.
//
// The alive handler answers after 200ms. The poll budget is shrunk to 50ms, so
// the idle-branch probe times out before the answer and returns probeUnknown; the
// row keeps its last-known liveness (LiveRunning). If the poll probe had
// inherited the operator budget (30s) it would WAIT for the 200ms answer, observe
// probeAlive, and settle the idle row to LiveReady — so asserting LiveRunning
// (not LiveReady) proves the short poll budget is still in effect. This is the
// regression guard that distinguishes the surgical fix (poll keeps the short
// budget) from a wholesale fix that loosened probeLiveness for every caller.
func TestRefreshStatuses_PollIdleProbeKeepsShortPollBudget(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, 50*time.Millisecond)
	// Explicit: the operator budget must NOT feed back into the poll idle probe.
	withRemoteConfirmProbeTimeout(t, 30*time.Second)

	manager, repoID, repoPath := newStatusTestManager(t)
	srv := delayedAliveRemoteServer(t, 200*time.Millisecond)
	inst, _ := registerStartedRemote(t, manager, repoID, repoPath, "remote-idle-probe-budget", srv.URL, session.Running)
	require.Equal(t, session.LiveRunning, inst.GetLiveness())

	manager.RefreshStatuses()

	require.Equal(t, session.LiveRunning, inst.GetLiveness(),
		"the poll's idle-branch probe used the SHORT poll budget (50ms) and timed out before the 200ms answer "+
			"(probeUnknown preserves last-known liveness); LiveReady here would mean the poll probe inherited the "+
			"30s operator budget and waited for the answer — the #1794 serial-walk stall the split exists to prevent")
}

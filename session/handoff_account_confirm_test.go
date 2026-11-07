package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// confirmableManualSwap is the manual account swap a #4429 confirm acts on: the
// replacement panes started, the mission verdict is recorded, and the admitted
// launch plan is still held — exactly what a resend that could not confirm
// leaves behind.
func confirmableManualSwap(status PromptDeliveryStatus) *Instance {
	return &Instance{
		Title:    "swapped",
		Account:  "personal",
		started:  true,
		liveness: LiveRunning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			Mission: "continue", MissionDeliveryStatus: status,
		},
		accountSwapLaunch: &accountSwapLaunchPlan{account: "personal", base: "claude", program: "claude"},
	}
}

// Confirming a manual account swap retires the whole transaction. The normal
// completion clears the pending swap AND its launch plan, and
// tabSpawnBlockedLocked refuses on either, so a confirm that left the plan
// behind kept every new tab refused as "account swap in progress" until the
// daemon restarted.
func TestConfirmPendingAccountSwapDeliveryReleasesTabSpawn(t *testing.T) {
	for _, status := range []PromptDeliveryStatus{PromptSentUnverified, PromptCouldNotConfirm, PromptDelivered} {
		for _, lv := range []struct {
			name string
			lv   Liveness
		}{
			{"from running", LiveRunning},
			{"from ready", LiveReady},
		} {
			t.Run(string(status)+lv.name, func(t *testing.T) {
				inst := confirmableManualSwap(status)
				inst.liveness = lv.lv
				require.Error(t, inst.TabSpawnBlocked(), "fixture: the pending swap blocks tab creation")
				require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

				require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

				swapPending, _ := inst.PendingManualAccountSwap()
				require.False(t, swapPending, "the confirm retires the pending swap")
				require.Nil(t, inst.accountSwapLaunch, "the confirm retires the admitted launch plan with it")
				require.NoError(t, inst.TabSpawnBlocked(),
					"a confirmed swap must not keep refusing new tabs")
				require.Equal(t, LiveRunning, inst.GetLiveness(),
					"a confirmed delivery is a working session (#5023): the agent already has its mission, "+
						"so the row reads running until the monitor's own idle observation")
			})
		}
	}
}

// The startup-unknown and orphaned-fence shapes are the ones this verb exists
// for: the daemon has probed the pane, so the confirm lifts both. A row parked
// at the INCOMING identity's own usage-limit wall keeps LiveLimitReached
// (#5023, ParkManualAccountSwapAtLimit): it is already not-idle to fleet
// watch, and ResumeLimitedSessions only owns rows still carrying it —
// flattening it to LiveRunning would strand the session while an attached
// client's paused poll never re-detects the wall.
func TestConfirmPendingAccountSwapDeliveryResolvesTheWedge(t *testing.T) {
	inst := confirmableManualSwap(PromptSentUnverified)
	inst.startupStateUnknown = true
	inst.started = false
	inst.inFlightOp = OpRespawning
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	// The wall is attributed to the incoming identity (ParkManualAccountSwapAtLimit
	// shape, limit_account == pending.To): the confirm keeps it.
	inst.limitAgent = "claude"
	inst.limitAccount = "personal"
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

	require.False(t, inst.StartupStateUnknown(), "the probe-backed confirm resolves the unknown flag")
	require.True(t, inst.Started(), "and restores the started bit the flag lowered")
	require.Equal(t, OpNone, inst.GetInFlightOp(), "the orphaned respawn fence is dropped")
	require.Equal(t, LiveLimitReached, inst.GetLiveness(),
		"a confirmed delivery on a row parked at the incoming identity's wall keeps it — ResumeLimitedSessions owns it")
	if reset, ok := inst.LimitResetAt(); !ok || reset.IsZero() {
		t.Fatal("the limit reset time must survive the confirm or the resume pass loses its schedule")
	}
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

// The limit-wall settle splits on the wall's ATTRIBUTION (#6799): a swap of
// either kind parked at the incoming identity's own wall keeps it, but an
// AUTOMATIC swap parked at the OUTGOING identity's wall — the shape this
// test's limit_account == pending.From fixture carries — retires it with
// the transaction: a notice the operator just confirmed delivered is
// already running on the new account's quota.
func TestConfirmPendingAccountSwapDeliveryAutomaticClearsTheOutgoingWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	inst.pendingAccountSwap.Manual = false
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	inst.limitAgent = "claude"
	inst.limitAccount = "work"
	inst.taskRunIdleEdgeHeld = true
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"the confirmed notice is already running on the incoming identity's quota")
	if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
		t.Fatal("the outgoing identity's wall retires with the swap")
	}
	require.False(t, inst.taskRunIdleEdgeHeld,
		"a stale held edge must not swallow the run's real completion edge")
	require.Nil(t, inst.pendingAccountSwap)
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

// The #6799 regression: the incoming-wall preservation must extend to the
// AUTOMATIC kind. settleReplacementRuntime parks the row with
// ParkAutomaticAccountSwapAtLimit when the replacement's own readiness wait
// hits its quota — attributing the wall to the incoming identity
// (limit_account == pending.To) while the notice verdict stays ambiguous. An
// operator's truthful "the notice landed" confirm retires the transaction,
// but must keep that wall: the agent really is quota-blocked, the parked row
// already publishes not-idle, and only a row still carrying LiveLimitReached
// stays inside ResumeLimitedSessions' scan. Flattening it strands the
// session outside scheduled recovery.
func TestConfirmPendingAccountSwapDeliveryAutomaticKeepsTheIncomingWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	inst.pendingAccountSwap.Manual = false
	attemptAt := time.Now().Add(-time.Minute)
	inst.pendingAccountSwap.MissionDeliveryAttemptAt = attemptAt
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	// The wall is attributed to the incoming identity — the
	// ParkAutomaticAccountSwapAtLimit shape, limit_account == pending.To —
	// and its sighting POSTDATES the recorded send: a later retry's
	// readiness wait hit the replacement's own quota after this attempt
	// landed (#6921 review — only a post-send wall is fresh quota proof).
	inst.limitAgent = "claude"
	inst.limitAccount = "personal"
	inst.accountLimitObservations = []AccountLimitObservationData{
		{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: attemptAt.Add(time.Second)},
	}
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

	require.Nil(t, inst.pendingAccountSwap, "the confirm retires the pending swap")
	require.Nil(t, inst.accountSwapLaunch)
	require.Equal(t, LiveLimitReached, inst.GetLiveness(),
		"an incoming-account limit recorded after the send survives the confirm — the agent really is quota-blocked")
	if reset, ok := inst.LimitResetAt(); !ok || reset.IsZero() {
		t.Fatal("the incoming wall's reset must survive or scheduled recovery loses its schedule")
	}
	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok, "the wall's attribution survives with it")
	require.Equal(t, "claude", agent)
	require.Equal(t, "personal", account)
}

// The #6921-review MED fix at the confirm site: a wall attributed to the
// incoming identity but sighted BEFORE the send being confirmed is stale —
// the send demonstrably got past it (the automatic arm never clears the
// limit before submitting, so the parked row outlives the retry) — and the
// confirm must clear it the way master did rather than keep a wall the
// scheduler would resume a working agent against.
func TestConfirmPendingAccountSwapDeliveryAutomaticClearsAPreSendIncomingWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	inst.pendingAccountSwap.Manual = false
	attemptAt := time.Now().Add(-time.Minute)
	inst.pendingAccountSwap.MissionDeliveryAttemptAt = attemptAt
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	inst.limitAgent = "claude"
	inst.limitAccount = "personal" // == pending.To on the label axis — but OLD
	inst.accountLimitObservations = []AccountLimitObservationData{
		{Agent: "claude", Account: "personal", ResetAt: inst.limitResetAt, ObservedAt: attemptAt.Add(-time.Hour)},
	}
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

	require.Nil(t, inst.pendingAccountSwap)
	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"a wall recorded before the confirmed send is stale and retires with the swap")
	if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
		t.Fatal("the pre-send wall must retire with the confirm")
	}
	_, _, ok := inst.LimitIdentity()
	require.False(t, ok, "no attribution may survive the cleared pre-send wall")
}

// A MANUAL swap confirmed while parked at the OUTGOING ambient identity's wall
// (the #6764 shape: limit_account == "" != pending.To) must clear the wall.
// The confirm nils the PendingAccountSwap pointer that
// AccountLimitEvidenceFromData guards against re-attributing the wall to the
// committed replacement, so keeping it would mint a durable quota observation
// for the healthy incoming account on every re-derivation — the exact #6764
// regression.
func TestConfirmPendingAccountSwapDeliveryManualClearsTheOutgoingAmbientWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	inst.pendingAccountSwap.From = "" // ambient outgoing identity
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	// The wall is the outgoing ambient identity's: limit_account == "" != pending.To.
	inst.limitAgent = "claude"
	inst.limitAccount = ""
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("", "personal"))

	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"the outgoing ambient wall retires with the confirmed swap — the replacement is already running")
	if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
		t.Fatal("the outgoing identity's wall retires with the swap")
	}
	require.Nil(t, inst.pendingAccountSwap)
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

// A MANUAL swap confirmed while parked at a named OUTGOING identity's wall
// (limit_account == "work" != pending.To "personal") must likewise clear the
// wall: the confirm retires the transaction, and the wall's attribution to the
// outgoing identity is stale once the replacement is already running.
func TestConfirmPendingAccountSwapDeliveryManualClearsTheNamedOutgoingWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	// confirmableManualSwap has From: "work", To: "personal", Account: "personal".
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	inst.limitAgent = "claude"
	inst.limitAccount = "work" // named outgoing wall, != pending.To "personal"
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))

	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"the outgoing identity's wall retires with the confirmed swap — the replacement is already running")
	if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
		t.Fatal("the outgoing identity's wall retires with the swap")
	}
	require.Nil(t, inst.pendingAccountSwap)
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

// A MANUAL cross-agent swap that reuses the outgoing label in a different
// provider's namespace (Claude `work` → Codex `work`, the shape exercised in
// daemon/handoff_account_round12_test.go) must still retire an outgoing wall
// even though limitAccount == pending.To: the wall's agent namespace
// (limitAgent "claude") does not match the transaction's incoming AccountAgent
// ("codex"), so it is the outgoing provider's wall, not the replacement's.
func TestConfirmPendingAccountSwapDeliveryManualClearsTheCrossAgentSameLabelWall(t *testing.T) {
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	// Same account label on both sides of a cross-agent swap; the incoming
	// agent namespace is Codex.
	inst.pendingAccountSwap.From = "work"
	inst.pendingAccountSwap.To = "work"
	inst.pendingAccountSwap.AccountAgent = tmux.ProgramCodex
	inst.Account = "work"
	inst.liveness = LiveLimitReached
	inst.limitResetAt = time.Now().Add(time.Hour)
	// The wall is the outgoing Claude identity's: same label, different agent.
	inst.limitAgent = tmux.ProgramClaude
	inst.limitAccount = "work"
	require.True(t, inst.CanConfirmPendingAccountSwapDelivery())

	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("work", "work"))

	require.Equal(t, LiveRunning, inst.GetLiveness(),
		"the outgoing provider's wall retires with the confirmed cross-agent swap — the replacement is already running")
	if reset, ok := inst.LimitResetAt(); ok || !reset.IsZero() {
		t.Fatal("the outgoing identity's wall retires with the swap")
	}
	require.Nil(t, inst.pendingAccountSwap)
	require.Nil(t, inst.accountSwapLaunch)
	require.NoError(t, inst.TabSpawnBlocked())
}

// The #6764 end-to-end regression: confirming a MANUAL swap parked at the
// outgoing ambient wall must not leave a row whose evidence re-derivation
// mints a durable quota observation for the healthy incoming account. The
// confirm clears the wall (so AccountLimitEvidenceFromData returns no
// observation), and a daemon restart reloads the row without the wall or the
// invented observation — the #6764 symptom does not materialize.
func TestConfirmPendingAccountSwapDeliveryManualOutgoingWallEvidenceNotMinted(t *testing.T) {
	resetAt := time.Date(2026, 11, 3, 6, 34, 0, 0, time.UTC)
	inst := confirmableManualSwap(PromptCouldNotConfirm)
	inst.pendingAccountSwap.From = "" // ambient outgoing identity
	inst.pendingAccountSwap.ReplacementPanesStarted = true
	inst.Program = tmux.ProgramCodex
	inst.liveness = LiveLimitReached
	inst.limitResetAt = resetAt
	inst.limitAgent = "claude"
	inst.limitAccount = "" // outgoing ambient wall, != pending.To "personal"

	// Pre-confirm: the swap pointer is still present, so the #6764 guard in
	// AccountLimitEvidenceFromData holds — the wall is attributed to the
	// outgoing identity, not the replacement.
	preData := inst.ToInstanceData().ForStorage()
	preAccount, preObs := AccountLimitEvidenceFromData(preData)
	require.Equal(t, "", preAccount,
		"pre-confirm: the wall is attributed to the outgoing identity, not the replacement")
	require.Empty(t, preObs,
		"pre-confirm: no observation is minted for the incoming identity")

	// Confirm the swap: retires the transaction, nils the swap pointer, and
	// clears the outgoing wall.
	require.NoError(t, inst.ConfirmPendingAccountSwapDelivery("", "personal"))

	// Post-confirm: the wall is cleared, so the evidence function cannot mint
	// a wrong observation for the healthy incoming account — even though the
	// swap pointer that guarded against re-attribution is now nil.
	postData := inst.ToInstanceData().ForStorage()
	postAccount, postObs := AccountLimitEvidenceFromData(postData)
	require.Equal(t, "", postAccount,
		"post-confirm: no wall is attributed to the replacement account")
	require.Empty(t, postObs,
		"post-confirm: no observation is minted for the incoming identity")

	// A daemon restart reloads the row without the wall and without the
	// invented observation — the #6764 symptom does not materialize.
	// StartupStateUnknown keeps the reload inert so FromInstanceData does not
	// try to re-attach a tmux session (the test has no live runtime); it does
	// not affect the evidence derivation, which runs before the inert return.
	postData.Path = t.TempDir()
	postData.Worktree = GitWorktreeData{
		RepoPath:     t.TempDir(),
		WorktreePath: t.TempDir(),
		SessionName:  "swapped",
		BranchName:   "af/swapped",
	}
	postData.StartupStateUnknown = true
	reloaded, err := FromInstanceData(postData)
	require.NoError(t, err)
	_, _, ok := reloaded.LimitIdentity()
	require.False(t, ok, "a restart must not load the replacement as limited")
	require.Empty(t, reloaded.AccountLimitObservations(),
		"a restart must not retain an invented observation for the replacement")
}

func TestCanConfirmPendingAccountSwapDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
		want   bool
	}{
		{name: "sent-unverified on a running row", mutate: func(*Instance) {}, want: true},
		{name: "could-not-confirm", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptCouldNotConfirm }, want: true},
		{name: "delivered crash window", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptDelivered }, want: true},
		{name: "ready", mutate: func(i *Instance) { i.liveness = LiveReady }, want: true},
		{name: "limit reached", mutate: func(i *Instance) { i.liveness = LiveLimitReached }, want: true},
		{name: "startup unknown", mutate: func(i *Instance) { i.startupStateUnknown = true; i.liveness = LivenessUnset }, want: true},
		{name: "orphaned respawn fence", mutate: func(i *Instance) { i.inFlightOp = OpRespawning }, want: true},
		// An automatic swap's ambiguous notice owns the same no-resend exit
		// (#6767): the re-verification window may have closed unanswered or
		// never armed, and the operator who can see it landed still attests.
		{name: "automatic swap", mutate: func(i *Instance) { i.pendingAccountSwap.Manual = false }, want: true},

		{name: "not delivered belongs to automatic recovery", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "no verdict recorded", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = "" }},
		{name: "replacement panes never started", mutate: func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{name: "no pending swap", mutate: func(i *Instance) { i.pendingAccountSwap = nil }},
		{name: "kill tombstone", mutate: func(i *Instance) { i.userKilled = true }},
		{name: "lost", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "lost and startup unknown", mutate: func(i *Instance) { i.liveness = LiveLost; i.startupStateUnknown = true }},
		{name: "dead", mutate: func(i *Instance) { i.liveness = LiveDead }},
		{name: "archived", mutate: func(i *Instance) { i.liveness = LiveArchived }},
		{name: "no proven liveness", mutate: func(i *Instance) { i.liveness = LivenessUnset }},
		{name: "another op in flight", mutate: func(i *Instance) { i.inFlightOp = OpKilling }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptSentUnverified)
			tc.mutate(inst)
			require.Equal(t, tc.want, inst.CanConfirmPendingAccountSwapDelivery())
		})
	}
}

// TestCanRetryPendingManualAccountSwapDelivery is the retry-predicate
// counterpart of TestCanConfirmPendingAccountSwapDelivery. Unlike the
// agent-handoff twin (CanRetryPendingHandoffMissionDelivery) it admits the
// limit wall: the daemon's manual-swap ResumeFromLimit fork re-pastes the
// pending mission (deliverManualAccountMission), so a row parked at the wall
// whose mission verdict is ambiguous is an honest resend, not a no-resend
// restart. The parked row a prior limit-during-delivery left at
// LiveLimitReached + PromptNotDelivered still refuses — that verdict belongs
// to automatic recovery, and the picker never opens on it.
func TestCanRetryPendingManualAccountSwapDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
		want   bool
	}{
		{name: "ambiguous on a running row", mutate: func(*Instance) {}, want: true},
		{name: "ready", mutate: func(i *Instance) { i.liveness = LiveReady }, want: true},
		{name: "limit reached — the resume re-sends the pending mission, so the row is an honest retry", mutate: func(i *Instance) { i.liveness = LiveLimitReached }, want: true},
		{name: "sent-unverified verdict", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptSentUnverified }, want: true},
		{name: "delivered crash window", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptDelivered }, want: true},

		{name: "startup-unknown at the limit — the committed swap's explicit retry is the inspection the marker waits for", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.startupStateUnknown = true
		}, want: true},
		{name: "startup-unknown on a running row — the committed swap still resends", mutate: func(i *Instance) { i.startupStateUnknown = true }, want: true},
		{name: "retry in progress", mutate: func(i *Instance) { i.inFlightOp = OpRespawning }},
		{name: "kill tombstone", mutate: func(i *Instance) { i.userKilled = true }},
		{name: "lost", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "dead", mutate: func(i *Instance) { i.liveness = LiveDead }},
		{name: "archived", mutate: func(i *Instance) { i.liveness = LiveArchived }},
		{name: "no proven liveness", mutate: func(i *Instance) { i.liveness = LivenessUnset }},
		{name: "not delivered belongs to automatic recovery", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "not delivered at the limit wall stays with automatic recovery", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered
		}},
		{name: "no verdict recorded", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = "" }},
		{name: "automatic swap", mutate: func(i *Instance) { i.pendingAccountSwap.Manual = false }},
		{name: "replacement panes never started", mutate: func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{name: "no pending swap", mutate: func(i *Instance) { i.pendingAccountSwap = nil }},
		{name: "another op in flight", mutate: func(i *Instance) { i.inFlightOp = OpKilling }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptCouldNotConfirm)
			tc.mutate(inst)
			require.Equal(t, tc.want, inst.CanRetryPendingManualAccountSwapDelivery())
		})
	}
}

// TestCanRetryPendingAccountSwapNoticeDelivery is the automatic swap's half
// of TestCanRetryPendingManualAccountSwapDelivery (#6800): the daemon's
// ResumeFromLimit fork re-submits the committed notice
// (deliverAutomaticAccountNotice) — but only for a row parked at its limit
// wall. resumeFromLimitLockedOutcome returns unperformed for a non-limited
// automatic swap, so the predicate mirrors the daemon's admission rather than
// copying the manual twin's wider liveness gate: a running/ready row, or a
// startup-unknown row off the wall, must not offer a resend the daemon will
// not perform. A startup-unknown row AT the wall is still an honest retry —
// the committed swap's BeginLimitResume bypasses the marker because the
// explicit retry is the inspection it waits for. Positive non-delivery stays
// with the scheduler, and every refusal keeps the verb hidden so the picker
// cannot lie about what Row 0 does.
func TestCanRetryPendingAccountSwapNoticeDelivery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Instance)
		want   bool
	}{
		{name: "limit reached — the resume re-sends the notice, so the row is an honest retry", mutate: func(i *Instance) { i.liveness = LiveLimitReached }, want: true},
		{name: "startup-unknown at the limit — the committed swap's explicit retry is the inspection the marker waits for", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.startupStateUnknown = true
		}, want: true},
		{name: "sent-unverified verdict at the wall", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.MissionDeliveryStatus = PromptSentUnverified
		}, want: true},
		{name: "delivered crash window at the wall", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.MissionDeliveryStatus = PromptDelivered
		}, want: true},

		{name: "ambiguous on a running row — the daemon's automatic resume no-ops off the limit wall", mutate: func(*Instance) {}},
		{name: "ready — same daemon no-op", mutate: func(i *Instance) { i.liveness = LiveReady }},
		{name: "startup-unknown off the wall — the daemon's automatic resume no-ops", mutate: func(i *Instance) { i.startupStateUnknown = true }},
		{name: "retry in progress", mutate: func(i *Instance) { i.liveness = LiveLimitReached; i.inFlightOp = OpRespawning }},
		{name: "kill tombstone", mutate: func(i *Instance) { i.liveness = LiveLimitReached; i.userKilled = true }},
		{name: "lost", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "dead", mutate: func(i *Instance) { i.liveness = LiveDead }},
		{name: "archived", mutate: func(i *Instance) { i.liveness = LiveArchived }},
		{name: "no proven liveness", mutate: func(i *Instance) { i.liveness = LivenessUnset }},
		{name: "not delivered belongs to automatic recovery", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "not delivered at the limit wall stays with automatic recovery", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered
		}},
		{name: "no verdict recorded", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.MissionDeliveryStatus = ""
		}},
		{name: "manual swap at the wall uses the twin predicate", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.Manual = true
		}},
		{name: "replacement panes never started", mutate: func(i *Instance) {
			i.liveness = LiveLimitReached
			i.pendingAccountSwap.ReplacementPanesStarted = false
		}},
		{name: "no pending swap", mutate: func(i *Instance) { i.liveness = LiveLimitReached; i.pendingAccountSwap = nil }},
		{name: "another op in flight", mutate: func(i *Instance) { i.liveness = LiveLimitReached; i.inFlightOp = OpKilling }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptCouldNotConfirm)
			inst.pendingAccountSwap.Manual = false
			tc.mutate(inst)
			require.Equal(t, tc.want, inst.CanRetryPendingAccountSwapNoticeDelivery())
		})
	}
}

// Every refusal leaves the transaction exactly as it was: a confirm that
// fails must not half-retire the swap or its launch plan.
func TestConfirmPendingAccountSwapDeliveryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		mutate   func(*Instance)
	}{
		{name: "different source account", from: "other", to: "personal", mutate: func(*Instance) {}},
		{name: "different target account", from: "work", to: "other", mutate: func(*Instance) {}},
		{name: "no replacement panes", from: "work", to: "personal", mutate: func(i *Instance) { i.pendingAccountSwap.ReplacementPanesStarted = false }},
		{name: "not delivered", from: "work", to: "personal", mutate: func(i *Instance) { i.pendingAccountSwap.MissionDeliveryStatus = PromptNotDelivered }},
		{name: "kill tombstone", from: "work", to: "personal", mutate: func(i *Instance) { i.userKilled = true }},
		{name: "lost", from: "work", to: "personal", mutate: func(i *Instance) { i.liveness = LiveLost }},
		{name: "busy", from: "work", to: "personal", mutate: func(i *Instance) { i.inFlightOp = OpArchiving }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := confirmableManualSwap(PromptSentUnverified)
			tc.mutate(inst)
			pendingBefore := *inst.pendingAccountSwap
			opBefore := inst.inFlightOp

			require.Error(t, inst.ConfirmPendingAccountSwapDelivery(tc.from, tc.to))

			require.NotNil(t, inst.pendingAccountSwap, "a refused confirm keeps the pending swap")
			require.Equal(t, pendingBefore, *inst.pendingAccountSwap)
			require.NotNil(t, inst.accountSwapLaunch, "a refused confirm keeps the launch plan")
			require.Equal(t, opBefore, inst.inFlightOp)
		})
	}
	t.Run("no pending swap", func(t *testing.T) {
		inst := confirmableManualSwap(PromptSentUnverified)
		inst.pendingAccountSwap = nil
		require.Error(t, inst.ConfirmPendingAccountSwapDelivery("work", "personal"))
	})
}

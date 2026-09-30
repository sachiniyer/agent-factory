package session

import "testing"

func TestPendingHandoffMissionReconstructsDurableFence(t *testing.T) {
	data := InstanceData{
		ID:                    "handoff-pending-id",
		Title:                 "handoff-pending",
		Program:               "claude",
		Status:                Running,
		Liveness:              LiveRunning,
		BackendType:           "docker",
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: PromptNotDelivered,
		TaskRunActive:         true,
	}

	stored := data.ForStorage()
	if stored.InFlightOp != OpNone {
		t.Fatalf("storage retained generic op %v, want OpNone", stored.InFlightOp)
	}
	if activity, _ := ClassifyActivity(stored); activity != ActivityPending {
		t.Fatalf("raw pending-handoff record classified as %v, want pending", activity)
	}

	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatalf("FromInstanceData: %v", err)
	}
	if got := restored.GetInFlightOp(); got != OpReplacing {
		t.Fatalf("restored pending-handoff op = %v, want OpReplacing", got)
	}
	if got := restored.PendingHandoffMission(); got != data.PendingHandoffMission {
		t.Fatalf("restored pending mission = %q, want %q", got, data.PendingHandoffMission)
	}

	// #4429: only verdicts whose obligation the daemon still resolves itself
	// reconstruct the fence. An ambiguous verdict's swap already completed — the
	// send path proved the incoming runtime before recording it — so the row
	// must not load fenced into the inert shape this fix exists to dismantle.
	// A sandbox record is the right fixture here and no more: its container is
	// genuinely gone after a restart, so it loads inert-Lost and restore owns
	// it — the confirm/retry exits are for a row whose runtime still answers
	// (the local-record case), which the in-memory wedge tests above exercise.
	ambiguous := data
	ambiguous.HandoffDeliveryStatus = PromptSentUnverified
	restoredAmbiguous, err := FromInstanceData(ambiguous.ForStorage())
	if err != nil {
		t.Fatalf("FromInstanceData(ambiguous): %v", err)
	}
	if got := restoredAmbiguous.GetInFlightOp(); got != OpNone {
		t.Fatalf("ambiguous pending record restored op %v, want OpNone — the operator needs a usable row", got)
	}
	if restoredAmbiguous.PendingHandoffMission() == "" {
		t.Fatal("the ambiguous pending mission must survive the round-trip")
	}
	if got := restoredAmbiguous.GetLiveness(); got != LiveLost {
		t.Fatalf("sandbox record loaded liveness %v, want LiveLost", got)
	}
	if restoredAmbiguous.CanConfirmPendingHandoffDelivery() || restoredAmbiguous.CanRetryPendingHandoffMissionDelivery() {
		t.Fatal("a runtime that no longer exists must refuse both exits — restore owns this row")
	}

	// A readiness failure deliberately converts the same pending record into the
	// stronger startup-unknown terminal state. That state must stay settled and
	// explicitly killable after reload, not reconstruct OpReplacing and hide its
	// only teardown handle.
	uncertain := data
	uncertain.StartupStateUnknown = true
	uncertain = uncertain.ForStorage()
	restoredUnknown, err := FromInstanceData(uncertain)
	if err != nil {
		t.Fatalf("FromInstanceData(startup unknown): %v", err)
	}
	if got := restoredUnknown.GetInFlightOp(); got != OpNone {
		t.Fatalf("startup-unknown pending record restored op %v, want OpNone", got)
	}
	if !restoredUnknown.CanKill() {
		t.Fatal("startup-unknown pending record lost its explicit kill handle")
	}
}

func TestPendingHandoffMissionWithoutEvidenceFailsClosed(t *testing.T) {
	data := InstanceData{
		ID:                    "legacy-pending-id",
		Title:                 "legacy-pending",
		Program:               "claude",
		Status:                Running,
		Liveness:              LiveRunning,
		BackendType:           "docker",
		PendingHandoffMission: "continue the inherited work",
	}
	normalized := data.restoreMissingHandoffMissionEvidence()
	if got := normalized.HandoffDeliveryStatus; got != PromptCouldNotConfirm {
		t.Fatalf("legacy pending mission evidence = %q, want %q", got, PromptCouldNotConfirm)
	}
	retryable := &Instance{
		liveness:              LiveRunning,
		pendingHandoffMission: normalized.PendingHandoffMission,
		handoffDeliveryStatus: normalized.HandoffDeliveryStatus,
	}
	if !retryable.CanRetryPendingHandoffMissionDelivery() {
		t.Fatal("normalized legacy evidence must expose explicit retry on a known live pane")
	}
	restored, err := FromInstanceData(data)
	if err != nil {
		t.Fatal(err)
	}
	if restored.PendingHandoffMissionAutoRetryable() {
		t.Fatal("a legacy pending mission without durable delivery evidence authorized automatic replay")
	}
}

func TestAmbiguousPendingHandoffProjectsRollbackFence(t *testing.T) {
	data := InstanceData{
		ID:                    "ambiguous-handoff-id",
		Title:                 "ambiguous-handoff",
		Program:               "claude",
		Status:                Running,
		Liveness:              LiveRunning,
		BackendType:           "docker",
		PendingHandoffMission: "continue the inherited work",
		HandoffDeliveryStatus: PromptCouldNotConfirm,
	}

	stored := data.ForStorage()
	if !stored.StartupStateUnknown {
		t.Fatal("an older reader must see an ambiguous pending handoff as startup-unknown")
	}

	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatal(err)
	}
	if restored.StartupStateUnknown() {
		t.Fatal("a current reader must restore the real known startup state")
	}
	if got := restored.ToInstanceData().HandoffDeliveryStatus; got != PromptCouldNotConfirm {
		t.Fatalf("current reader lost mission-scoped evidence: got %q", got)
	}

	notDelivered := data
	notDelivered.HandoffDeliveryStatus = PromptNotDelivered
	if got := notDelivered.ForStorage(); got.StartupStateUnknown {
		t.Fatalf("positive non-delivery must remain automatically recoverable, got %+v", got)
	}
}

func TestPendingHandoffMissionExplicitRetryRequiresAmbiguousKnownRuntime(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         PromptDeliveryStatus
		startupUnknown bool
		lost           bool
		want           bool
	}{
		{name: "could not confirm", status: PromptCouldNotConfirm, want: true},
		{name: "sent unverified", status: PromptSentUnverified, want: true},
		{name: "positive non-delivery is automatic only", status: PromptNotDelivered},
		// #4429: startup-unknown is exactly the wedge shape the explicit retry
		// exits — the send path's readiness wait re-establishes the pane proof
		// the flag says is missing.
		{name: "startup unknown admits the explicit resend", status: PromptCouldNotConfirm, startupUnknown: true, want: true},
		{name: "lost runtime is not inspectable", status: PromptCouldNotConfirm, lost: true},
		{name: "lost plus startup unknown stays refused", status: PromptCouldNotConfirm, startupUnknown: true, lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := NewInstance(InstanceOptions{Title: "ambiguous-handoff", Path: t.TempDir(), Program: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			inst.SetBackend(NewFakeBackend())
			inst.SetStartedForTest(true)
			inst.SetStatusForTest(Running)
			if err := inst.Transition(BeginHandoff()); err != nil {
				t.Fatal(err)
			}
			mission := "continue the inherited work"
			inst.SetPendingHandoffMission(mission)
			if tc.status != PromptNotDelivered {
				if err := inst.BeginPendingHandoffMissionDelivery(mission); err != nil {
					t.Fatal(err)
				}
				if err := inst.RecordPendingHandoffMissionDelivery(mission, tc.status); err != nil {
					t.Fatal(err)
				}
			}
			if tc.startupUnknown {
				inst.MarkStartupStateUnknown()
			}
			if tc.lost {
				inst.SetStatusForTest(Lost)
			}
			if got := inst.CanRetryPendingHandoffMissionDelivery(); got != tc.want {
				t.Fatalf("CanRetryPendingHandoffMissionDelivery() = %v, want %v (liveness=%v op=%v startupUnknown=%v evidence=%q)",
					got, tc.want, inst.GetLiveness(), inst.GetInFlightOp(), inst.StartupStateUnknown(), inst.ToInstanceData().HandoffDeliveryStatus)
			}
		})
	}
}

// stageWedge builds the #4429 wedge: a swap that completed, a mission whose
// delivery reported ambiguous, and the replacement fence still raised.
func stageWedge(t *testing.T, status PromptDeliveryStatus, startupUnknown bool) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "wedged-handoff", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetBackend(NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(Running)
	if err := inst.Transition(BeginHandoff()); err != nil {
		t.Fatal(err)
	}
	mission := "continue the inherited work"
	inst.SetPendingHandoffMission(mission)
	if err := inst.BeginPendingHandoffMissionDelivery(mission); err != nil {
		t.Fatal(err)
	}
	if err := inst.RecordPendingHandoffMissionDelivery(mission, status); err != nil {
		t.Fatal(err)
	}
	if startupUnknown {
		inst.MarkStartupStateUnknown()
	}
	return inst
}

// TestConfirmPendingHandoffDeliveryReleasesTheWedge is the #4429 regression: a
// sent-unverified mission delivery must be resolvable through a supported
// command, not a store edit. The operator inspects the pane, sees the agent
// already acting on the mission, and attests — the confirmation clears the
// replacement fence, the mission, and (when present) the startup-unknown flag
// in one settle, leaving a live row the poll owns again.
func TestConfirmPendingHandoffDeliveryReleasesTheWedge(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         PromptDeliveryStatus
		startupUnknown bool
	}{
		{name: "sent-unverified under the replacement fence", status: PromptSentUnverified},
		{name: "could-not-confirm under the replacement fence", status: PromptCouldNotConfirm},
		{name: "delivered crash window under the replacement fence", status: PromptDelivered},
		{name: "sent-unverified with startup-unknown", status: PromptSentUnverified, startupUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := stageWedge(t, tc.status, tc.startupUnknown)
			if !inst.CanConfirmPendingHandoffDelivery() {
				t.Fatal("wedged row must advertise the confirm exit")
			}
			if err := inst.ConfirmPendingHandoffDelivery("continue the inherited work"); err != nil {
				t.Fatalf("ConfirmPendingHandoffDelivery: %v", err)
			}
			if got := inst.GetInFlightOp(); got != OpNone {
				t.Fatalf("op after confirm = %v, want OpNone", got)
			}
			if inst.StartupStateUnknown() {
				t.Fatal("confirm must clear the startup-unknown fence")
			}
			if got := inst.PendingHandoffMission(); got != "" {
				t.Fatalf("mission after confirm = %q, want retired", got)
			}
			if got := inst.GetLiveness(); got != LiveRunning {
				t.Fatalf("liveness after confirm = %v, want Running", got)
			}
			if got := inst.LifecycleAction(); got == LifecycleActionNone {
				t.Fatal("the wedged row must regain its lifecycle action after confirm")
			}
			if !inst.CanKill() {
				t.Fatal("the settled row must expose its kill handle")
			}

			// The durable record must round-trip without rebuilding the wedge:
			// no reconstructed fence, no compatibility projection. The sandbox
			// backend type skips the git-worktree restore leg, which needs a
			// real on-disk worktree this fixture does not create.
			stored := inst.ToInstanceData().ForStorage()
			if stored.StartupStateUnknown {
				t.Fatal("storage projected startup-unknown for a settled row")
			}
			stored.BackendType = "docker"
			restored, err := FromInstanceData(stored)
			if err != nil {
				t.Fatal(err)
			}
			if got := restored.GetInFlightOp(); got != OpNone {
				t.Fatalf("restored op = %v, want OpNone", got)
			}
			if restored.PendingHandoffMission() != "" || restored.StartupStateUnknown() {
				t.Fatal("restored record rebuilt the wedge")
			}
		})
	}
}

// TestConfirmPendingHandoffDeliveryRefusals keeps the confirm path honest: it
// is an attestation about a recorded ambiguous verdict, never a backdoor around
// the delivery state machine.
func TestConfirmPendingHandoffDeliveryRefusals(t *testing.T) {
	t.Run("no pending mission", func(t *testing.T) {
		inst, err := NewInstance(InstanceOptions{Title: "plain", Path: t.TempDir(), Program: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		if err := inst.ConfirmPendingHandoffDelivery("anything"); err == nil {
			t.Fatal("confirm with no pending mission must fail")
		}
	})
	t.Run("mismatched mission", func(t *testing.T) {
		inst := stageWedge(t, PromptSentUnverified, false)
		if err := inst.ConfirmPendingHandoffDelivery("a different mission"); err == nil {
			t.Fatal("confirm naming a different mission must fail")
		}
	})
	t.Run("not-delivered belongs to automatic recovery", func(t *testing.T) {
		inst := stageWedge(t, PromptNotDelivered, false)
		if inst.CanConfirmPendingHandoffDelivery() {
			t.Fatal("not-delivered must not advertise confirm")
		}
		if err := inst.ConfirmPendingHandoffDelivery("continue the inherited work"); err == nil {
			t.Fatal("confirming a known-absent mission must fail")
		}
	})
	t.Run("user killed", func(t *testing.T) {
		inst := stageWedge(t, PromptSentUnverified, false)
		inst.MarkUserKilled()
		if inst.CanConfirmPendingHandoffDelivery() {
			t.Fatal("a tombstoned row must not advertise confirm")
		}
		if err := inst.ConfirmPendingHandoffDelivery("continue the inherited work"); err == nil {
			t.Fatal("confirm under a kill tombstone must fail")
		}
	})
	t.Run("lost runtime is not confirmable", func(t *testing.T) {
		inst := stageWedge(t, PromptSentUnverified, false)
		inst.SetStatusForTest(Lost)
		if inst.CanConfirmPendingHandoffDelivery() {
			t.Fatal("a Lost row keeps the restore path, not the confirm attestation")
		}
	})
}

// TestExplicitRetryAdmitsStartupUnknown pins the #4429 extension: the explicit
// operator retry is the supported resend for the startup-unknown wedge shape —
// the send path's own readiness wait re-establishes the runtime proof the flag
// says is missing.
func TestExplicitRetryAdmitsStartupUnknown(t *testing.T) {
	inst := stageWedge(t, PromptSentUnverified, true)
	if inst.GetInFlightOp() != OpNone {
		t.Fatalf("startup-unknown wedge keeps op %v, want None", inst.GetInFlightOp())
	}
	if !inst.CanRetryPendingHandoffMissionDelivery() {
		t.Fatal("explicit retry must be offered on a startup-unknown pending row")
	}
	if inst.PendingHandoffMissionAutoRetryable() {
		t.Fatal("automatic replay must still refuse the ambiguous verdict")
	}
}

// TestResolveStartupStateRestoresStarted proves the shared resolve half of the
// confirm/retry settle: the unknown flag and the lifted started bit move back
// together so the row the probe just proved is the row the record describes.
func TestResolveStartupStateRestoresStarted(t *testing.T) {
	inst := stageWedge(t, PromptCouldNotConfirm, true)
	inst.ResolveStartupState()
	if inst.StartupStateUnknown() {
		t.Fatal("resolve must clear the unknown flag")
	}
	if !inst.Started() {
		t.Fatal("resolve must restore the started bit MarkStartupStateUnknown lifted")
	}
	if inst.PendingHandoffMission() == "" {
		t.Fatal("resolve alone must not retire the delivery obligation")
	}
}

// TestPendingHandoffMissionWithUserKilledDoesNotReconstructFence proves the
// durable kill tombstone outranks an undelivered handoff mission after storage
// has scrubbed the process-local operation axis. The row must remain settled and
// explicitly killable so the daemon can finish teardown after restart.
func TestPendingHandoffMissionWithUserKilledDoesNotReconstructFence(t *testing.T) {
	stored := (InstanceData{
		ID:                    "killed-during-handoff-id",
		Title:                 "killed-during-handoff",
		Program:               "claude",
		Status:                Running,
		Liveness:              LiveRunning,
		BackendType:           "docker",
		PendingHandoffMission: "continue the inherited work",
		UserKilled:            true,
	}).ForStorage()

	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatalf("FromInstanceData: %v", err)
	}
	if got := restored.GetInFlightOp(); got != OpNone {
		t.Fatalf("UserKilled pending-handoff record restored op %v, want OpNone", got)
	}
	if got := restored.GetStatus(); got == Loading {
		t.Fatalf("UserKilled pending-handoff record restored as %v, want a settled teardown row", got)
	}
	if !restored.CanKill() {
		t.Fatal("UserKilled pending-handoff record lost its explicit kill handle")
	}
	if got := restored.LifecycleAction(); got != LifecycleActionNone {
		t.Fatalf("UserKilled pending-handoff record advertised lifecycle action %q, want none", got)
	}

	// A live daemon snapshot can still carry the pre-kill replacement op. The
	// tombstone must override that carried transient just as it overrides the
	// durable mission's reconstructed transient after a disk round-trip.
	snapshot := stored
	snapshot.Status = Loading
	snapshot.InFlightOp = OpReplacing
	restoredSnapshot, err := FromInstanceData(snapshot)
	if err != nil {
		t.Fatalf("FromInstanceData(snapshot): %v", err)
	}
	if got := restoredSnapshot.GetInFlightOp(); got != OpNone {
		t.Fatalf("UserKilled pending-handoff snapshot restored op %v, want OpNone", got)
	}
	if !restoredSnapshot.CanKill() {
		t.Fatal("UserKilled pending-handoff snapshot lost its explicit kill handle")
	}
}

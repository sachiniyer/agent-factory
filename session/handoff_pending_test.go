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
		{name: "startup unknown is inert", status: PromptCouldNotConfirm, startupUnknown: true},
		{name: "lost runtime is not inspectable", status: PromptCouldNotConfirm, lost: true},
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

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
	uncertain := stored
	uncertain.StartupStateUnknown = true
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

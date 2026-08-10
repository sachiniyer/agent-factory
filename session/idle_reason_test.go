package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIdleReasonForUsesOnlyMechanicalFacts(t *testing.T) {
	t.Parallel()

	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	paneChangedAt := attemptedAt.Add(time.Second)

	tests := []struct {
		name string
		data InstanceData
		want IdleReason
	}{
		{
			name: "usage limit matcher won",
			data: InstanceData{Liveness: LiveLimitReached},
			want: IdleReasonUsageLimit,
		},
		{
			name: "process authoritatively lost",
			data: InstanceData{Liveness: LiveLost},
			want: IdleReasonProcessExited,
		},
		{
			name: "legacy process death",
			data: InstanceData{Liveness: LiveDead},
			want: IdleReasonProcessExited,
		},
		{
			name: "recognized recreate notice is pending",
			data: InstanceData{
				Liveness:            LiveReady,
				RootRecreateContext: RootRecreateContextFresh,
			},
			want: IdleReasonRecreatePending,
		},
		{
			name: "prompt was mechanically not delivered",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptNotDelivered,
			},
			want: IdleReasonPromptNotDelivered,
		},
		{
			name: "delivery could not be confirmed and pane did not change",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptCouldNotConfirm,
			},
			want: IdleReasonDeliveryUnconfirmed,
		},
		{
			name: "sent prompt could not be verified and pane did not change",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptSentUnverified,
			},
			want: IdleReasonDeliveryUnconfirmed,
		},
		{
			name: "delivered prompt has no later pane change",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptDelivered,
			},
			want: IdleReasonNoPaneChangeSinceDelivery,
		},
		{
			name: "delivered prompt followed by pane change settled",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptDelivered,
				LastPaneChurnAt:          paneChangedAt,
			},
			want: IdleReasonSettledAfterPaneChange,
		},
		{
			name: "unconfirmed long prompt followed by pane change settled",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptCouldNotConfirm,
				LastPaneChurnAt:          paneChangedAt,
			},
			want: IdleReasonSettledAfterPaneChange,
		},
		{
			name: "sent unverified prompt followed by pane change settled",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptSentUnverified,
				LastPaneChurnAt:          paneChangedAt,
			},
			want: IdleReasonSettledAfterPaneChange,
		},
		{
			name: "currently running does not receive an idle reason",
			data: InstanceData{
				Liveness:                 LiveRunning,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptDelivered,
			},
		},
		{
			name: "ready without prompt evidence stays unspecified",
			data: InstanceData{Liveness: LiveReady},
		},
		{
			name: "unknown recreate value is not interpreted",
			data: InstanceData{
				Liveness:            LiveReady,
				RootRecreateContext: RootRecreateContext("future-value"),
			},
		},
		{
			name: "unknown delivery value is not interpreted even after churn",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptDeliveryStatus("future-value"),
				LastPaneChurnAt:          paneChangedAt,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IdleReasonFor(tc.data); got != tc.want {
				t.Fatalf("IdleReasonFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPromptAttemptFencesOverlappingPaneObservation(t *testing.T) {
	t.Parallel()

	inst := &Instance{liveness: LiveReady}
	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if !inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), 0) {
		t.Fatal("setup pane churn was not recorded")
	}
	inst.RecordPromptAttempt(PromptDelivered, attemptedAt)

	reason, churnAt := inst.IdleReasonSnapshot()
	if reason != IdleReasonNoPaneChangeSinceDelivery || !churnAt.IsZero() {
		t.Fatalf("post-attempt evidence = (%q, %v), want fenced no-change evidence", reason, churnAt)
	}
	if inst.RecordPaneChurnAtEpoch(attemptedAt.Add(2*time.Second), 0) {
		t.Fatal("an observation from before prompt delivery crossed the evidence fence")
	}
}

func TestPaneChurnRequestsOneCheckpointPerPrompt(t *testing.T) {
	t.Parallel()

	inst := &Instance{liveness: LiveRunning}
	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(PromptDelivered, attemptedAt)
	epoch := inst.StateEpoch()

	recorded, checkpoint := inst.RecordPaneChurnCheckpointAtEpoch(attemptedAt.Add(time.Second), epoch)
	if !recorded || !checkpoint {
		t.Fatalf("first churn = (%v, %v), want recorded checkpoint", recorded, checkpoint)
	}
	recorded, checkpoint = inst.RecordPaneChurnCheckpointAtEpoch(attemptedAt.Add(2*time.Second), epoch)
	if !recorded || checkpoint {
		t.Fatalf("later churn = (%v, %v), want recorded without another checkpoint", recorded, checkpoint)
	}
}

func TestClearIdleEvidenceRetiresPredecessorRuntimeFacts(t *testing.T) {
	t.Parallel()

	inst := &Instance{liveness: LiveReady}
	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(PromptDelivered, attemptedAt)
	epoch := inst.StateEpoch()
	inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), epoch)

	if !inst.ClearIdleEvidence() {
		t.Fatal("runtime replacement did not clear predecessor evidence")
	}
	reason, churnAt := inst.IdleReasonSnapshot()
	if reason != IdleReasonNone || !churnAt.IsZero() {
		t.Fatalf("cleared evidence = (%q, %v), want empty", reason, churnAt)
	}
	if inst.RecordPaneChurnAtEpoch(attemptedAt.Add(2*time.Second), epoch) {
		t.Fatal("predecessor observation crossed the runtime replacement fence")
	}
}

func TestIdleReasonEvidenceRecordsWithoutSemanticInference(t *testing.T) {
	t.Parallel()

	inst := &Instance{liveness: LiveReady}
	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	churnAt := attemptedAt.Add(time.Second)

	if !inst.RecordPromptAttempt(PromptCouldNotConfirm, attemptedAt) {
		t.Fatal("first prompt attempt was not recorded")
	}
	if got, _ := inst.IdleReasonSnapshot(); got != IdleReasonDeliveryUnconfirmed {
		t.Fatalf("reason before churn = %q, want %q", got, IdleReasonDeliveryUnconfirmed)
	}
	epoch := inst.StateEpoch()
	if !inst.RecordPaneChurnAtEpoch(churnAt, epoch) {
		t.Fatal("same-epoch pane churn was not recorded")
	}
	if got, gotChurn := inst.IdleReasonSnapshot(); got != IdleReasonSettledAfterPaneChange || !gotChurn.Equal(churnAt) {
		t.Fatalf("reason after churn = (%q, %v), want (%q, %v)", got, gotChurn, IdleReasonSettledAfterPaneChange, churnAt)
	}
	if inst.RecordPaneChurnAtEpoch(churnAt.Add(time.Second), epoch-1) {
		t.Fatal("stale-runtime pane churn must not be recorded")
	}
}

func TestIdleReasonProjectionAndStorageBoundary(t *testing.T) {
	t.Parallel()

	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	data := InstanceData{
		Status:                   Ready,
		Liveness:                 LiveReady,
		LastPromptAttemptAt:      attemptedAt,
		LastPromptDeliveryStatus: PromptDelivered,
	}.ProjectIdleReason()
	if data.IdleReason != IdleReasonNoPaneChangeSinceDelivery {
		t.Fatalf("projected reason = %q", data.IdleReason)
	}

	wire, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"idle_reason":"no-pane-change-since-delivery"`) ||
		!strings.Contains(string(wire), `"last_prompt_delivery_status":"delivered"`) {
		t.Fatalf("projection omitted idle evidence: %s", wire)
	}
	if strings.Contains(string(wire), `"last_pane_churn_at"`) {
		t.Fatalf("projection emitted a nonexistent pane-churn time: %s", wire)
	}

	stored := data.ForStorage()
	if stored.IdleReason != IdleReasonNone {
		t.Fatalf("derived reason reached storage: %q", stored.IdleReason)
	}
	if !stored.LastPromptAttemptAt.Equal(attemptedAt) || stored.LastPromptDeliveryStatus != PromptDelivered {
		t.Fatalf("mechanical evidence was scrubbed from storage: %+v", stored)
	}

	withChurn := data
	withChurn.LastPaneChurnAt = attemptedAt.Add(time.Minute)
	retired := withChurn.WithoutIdleEvidence()
	if retired.IdleReason != IdleReasonNone || !retired.LastPromptAttemptAt.IsZero() ||
		retired.LastPromptDeliveryStatus != "" || !retired.LastPaneChurnAt.IsZero() {
		t.Fatalf("retired runtime evidence survived in checkpoint: %+v", retired)
	}
}

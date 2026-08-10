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
			name: "unknown delivery value is not interpreted",
			data: InstanceData{
				Liveness:                 LiveReady,
				LastPromptAttemptAt:      attemptedAt,
				LastPromptDeliveryStatus: PromptDeliveryStatus("future-value"),
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
	if !inst.RecordPaneChurnAtEpoch(churnAt, 0) {
		t.Fatal("same-epoch pane churn was not recorded")
	}
	if got, gotChurn := inst.IdleReasonSnapshot(); got != IdleReasonSettledAfterPaneChange || !gotChurn.Equal(churnAt) {
		t.Fatalf("reason after churn = (%q, %v), want (%q, %v)", got, gotChurn, IdleReasonSettledAfterPaneChange, churnAt)
	}
	if inst.RecordPaneChurnAtEpoch(churnAt.Add(time.Second), 1) {
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
}

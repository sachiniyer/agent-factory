package session

import (
	"testing"
	"time"
)

type observationFenceBackend struct {
	*FakeBackend
	snapshotStarted chan struct{}
	releaseSnapshot chan struct{}
}

func (b *observationFenceBackend) HasUpdated(*Instance) (bool, bool, string) {
	close(b.snapshotStarted)
	<-b.releaseSnapshot
	return true, false, "pre-delivery capture"
}

func (b *observationFenceBackend) SendPromptCommandWithStatus(*Instance, string) (PromptDeliveryStatus, error) {
	return PromptDelivered, nil
}

func TestPromptDeliveryWaitsForSnapshotAndFencesItsApply(t *testing.T) {
	backend := &observationFenceBackend{
		FakeBackend:     NewFakeBackend(),
		snapshotStarted: make(chan struct{}),
		releaseSnapshot: make(chan struct{}),
	}
	inst, err := NewInstance(InstanceOptions{Title: "fenced", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetBackend(backend)
	epoch := inst.StateEpoch()

	snapshotDone := make(chan error, 1)
	go func() {
		_, err := inst.SnapshotAgent(inst.AgentServer())
		snapshotDone <- err
	}()
	<-backend.snapshotStarted

	attemptedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	clockCalled := make(chan struct{})
	deliveryStarted := make(chan struct{})
	deliveryDone := make(chan error, 1)
	go func() {
		close(deliveryStarted)
		_, err := inst.SendPromptWithEvidence("ship it", func() time.Time {
			close(clockCalled)
			return attemptedAt
		})
		deliveryDone <- err
	}()
	<-deliveryStarted
	select {
	case <-clockCalled:
		t.Fatal("prompt attempt began before the older pane snapshot completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(backend.releaseSnapshot)
	if err := <-snapshotDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	if inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Second), epoch) {
		t.Fatal("pre-delivery snapshot crossed the prompt-attempt epoch fence")
	}
	reason, _ := inst.IdleReasonSnapshot()
	if reason != IdleReasonNoPaneChangeSinceDelivery {
		t.Fatalf("reason = %q, want %q", reason, IdleReasonNoPaneChangeSinceDelivery)
	}
}

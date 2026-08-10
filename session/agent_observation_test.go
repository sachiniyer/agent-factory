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

type deliveryWinsObservationBackend struct {
	*FakeBackend
	sendStarted chan struct{}
	releaseSend chan struct{}
}

func (b *deliveryWinsObservationBackend) HasUpdated(*Instance) (bool, bool, string) {
	return true, false, "post-delivery response"
}

func (b *deliveryWinsObservationBackend) SendPromptCommandWithStatus(*Instance, string) (PromptDeliveryStatus, error) {
	close(b.sendStarted)
	<-b.releaseSend
	return PromptDelivered, nil
}

func TestSnapshotAgentSamplesEpochInsideObservationFence(t *testing.T) {
	backend := &deliveryWinsObservationBackend{
		FakeBackend: NewFakeBackend(),
		sendStarted: make(chan struct{}),
		releaseSend: make(chan struct{}),
	}
	inst, err := NewInstance(InstanceOptions{Title: "delivery-wins", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetBackend(backend)
	oldEpoch := inst.StateEpoch()

	deliveryDone := make(chan error, 1)
	go func() {
		_, err := inst.SendPromptWithEvidence("ship it", time.Now)
		deliveryDone <- err
	}()
	<-backend.sendStarted

	type snapshotResult struct {
		op    InFlightOp
		epoch uint64
		err   error
	}
	snapshotDone := make(chan snapshotResult, 1)
	go func() {
		_, _, op, epoch, err := inst.SnapshotAgent()
		snapshotDone <- snapshotResult{op: op, epoch: epoch, err: err}
	}()
	close(backend.releaseSend)
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	got := <-snapshotDone
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.op != OpNone {
		t.Fatalf("snapshot op = %v, want none", got.op)
	}
	if got.epoch == oldEpoch || got.epoch != inst.StateEpoch() {
		t.Fatalf("snapshot epoch = %d, want post-delivery epoch %d (old %d)", got.epoch, inst.StateEpoch(), oldEpoch)
	}
	if !inst.RecordPaneChurnAtEpoch(time.Now(), got.epoch) {
		t.Fatal("post-delivery snapshot was rejected after consuming its pane hash")
	}
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
		_, _, _, _, err := inst.SnapshotAgent()
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

func TestRuntimeReplacementDoesNotWaitForPredecessorSnapshot(t *testing.T) {
	backend := &observationFenceBackend{
		FakeBackend:     NewFakeBackend(),
		snapshotStarted: make(chan struct{}),
		releaseSnapshot: make(chan struct{}),
	}
	inst, err := NewInstance(InstanceOptions{Title: "replacement", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetBackend(backend)

	snapshotDone := make(chan error, 1)
	go func() {
		_, _, _, _, err := inst.SnapshotAgent()
		snapshotDone <- err
	}()
	<-backend.snapshotStarted

	// Runtime replacement retires the predecessor's evidence. Delivery to the
	// replacement must not wait for pane I/O that is still blocked in the old
	// runtime; its eventual observation is rejected by the state-epoch fence.
	oldEpoch := inst.StateEpoch()
	inst.ClearIdleEvidence()
	deliveryDone := make(chan error, 1)
	go func() {
		_, err := inst.SendPromptWithEvidence("continue", time.Now)
		deliveryDone <- err
	}()
	select {
	case err := <-deliveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		close(backend.releaseSnapshot)
		<-snapshotDone
		<-deliveryDone
		t.Fatal("replacement delivery waited for the predecessor runtime's snapshot")
	}

	close(backend.releaseSnapshot)
	if err := <-snapshotDone; err != nil {
		t.Fatal(err)
	}
	if inst.RecordPaneChurnAtEpoch(time.Now(), oldEpoch) {
		t.Fatal("predecessor snapshot crossed the runtime-replacement epoch fence")
	}
}

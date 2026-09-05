package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

func TestPersistLoadRuntimeReplacementsCheckpointsEvidenceClear(t *testing.T) {
	_, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "restarted", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatusForTest(session.Ready)
	attemptedAt := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt)
	_, epoch := inst.InFlightOpAndEpoch()
	inst.RecordPaneChurnAtEpoch(attemptedAt.Add(time.Minute), epoch)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.LoadState().SaveInstances(repoID, seeded); err != nil {
		t.Fatal(err)
	}

	inst.ClearIdleEvidence()
	inst.MarkLoadRuntimeReplacedForTest()
	key := daemonInstanceKey(repoID, inst.Title)
	if owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst}); len(owed) != 0 {
		t.Fatalf("successful checkpoint left %d owed settlements", len(owed))
	}
	rec := recordFor(t, repoID, inst.Title)
	if rec == nil || !rec.LastPromptAttemptAt.IsZero() || rec.LastPromptDeliveryStatus != "" || !rec.LastPaneChurnAt.IsZero() {
		t.Fatalf("persisted load-time replacement = %+v; want cleared idle evidence", rec)
	}
	if inst.ConsumeLoadRuntimeReplacement() {
		t.Fatal("load-time replacement settlement was not consumed")
	}
}

// The session regression drives real sibling RestoreWithResult bookkeeping;
// this test pins the daemon half of that contract without clearing agent evidence.
func TestPersistLoadRuntimeReplacementsCheckpointsSiblingTimestamp(t *testing.T) {
	_, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "sibling-restarted", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatusForTest(session.Ready)
	attemptedAt := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt)
	seeded, err := json.Marshal([]session.InstanceData{inst.ToInstanceData()})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.LoadState().SaveInstances(repoID, seeded); err != nil {
		t.Fatal(err)
	}
	// Model the timestamp and settlement marker produced by a sibling respawn.
	inst.UpdatedAt = inst.UpdatedAt.Add(time.Hour)
	inst.MarkLoadRuntimeReplacedForTest()
	key := daemonInstanceKey(repoID, inst.Title)
	if owed := persistLoadRuntimeReplacements(map[string]*session.Instance{key: inst}); len(owed) != 0 {
		t.Fatalf("successful checkpoint left %d owed settlements", len(owed))
	}
	rec := recordFor(t, repoID, inst.Title)
	if rec == nil || !rec.UpdatedAt.Equal(inst.UpdatedAt) || !rec.LastPromptAttemptAt.Equal(attemptedAt) || rec.LastPromptDeliveryStatus != session.PromptDelivered {
		t.Fatalf("persisted sibling replacement = %+v; want updated timestamp and preserved agent evidence", rec)
	}
	if inst.ConsumeLoadRuntimeReplacement() {
		t.Fatal("settlement was not consumed")
	}
}

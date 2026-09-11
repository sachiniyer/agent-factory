package daemon

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// Exercise the advice at both manual refusal sites. The positive case prevents
// fixing the dead-end advice by always recommending kill (#4195).
func TestRestoreSession_ReapAdviceRequiresDurableBranch(t *testing.T) {
	for _, unreachable := range []bool{true, false} {
		path := "push_failure"
		if unreachable {
			path = "indeterminate"
		}
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				memoryBranch string
				storedBranch string
				storedID     string
				missing      bool
				unreadable   bool
				wantForce    bool
			}{
				{name: "unknown_branch", storedID: "current"},
				{name: "missing_record", memoryBranch: "af/current", missing: true},
				{name: "unrecorded_branch", memoryBranch: "af/current", storedID: "current"},
				{name: "blank_stored_branch", memoryBranch: "af/current", storedBranch: " \t", storedID: "current"},
				{name: "reused_title", memoryBranch: "af/current", storedBranch: "af/current", storedID: "previous"},
				{name: "stale_branch", memoryBranch: "af/current", storedBranch: "af/previous", storedID: "current"},
				{name: "record_read_error", memoryBranch: "af/current", unreadable: true},
				{name: "durable_matching_branch", memoryBranch: "af/current", storedBranch: "af/current", storedID: "current", wantForce: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					manager, repoID, repoPath := newStatusTestManager(t)
					srv := newSandboxProbeServer(t, "af/current")
					srv.unreachable.Store(unreachable)
					srv.archiveFails.Store(true)
					inst, backend, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "off-ramp", srv.url, session.Lost)
					inst.ID = "current"
					inst.SetSandboxBranch(tc.memoryBranch)
					records := []session.InstanceData{}
					if !tc.missing {
						rec := inst.ToInstanceData()
						rec.ID, rec.Branch = tc.storedID, tc.storedBranch
						records = append(records, rec)
					}
					raw, err := json.Marshal(records)
					require.NoError(t, err)
					if tc.unreadable {
						// Corrupt bytes fail the real record reader.
						raw = []byte(`{`)
					}
					require.NoError(t, config.SaveRepoInstances(repoID, raw))
					guardErr := requireDurableSandboxBranch(repoID, inst)
					if tc.wantForce {
						require.NoError(t, guardErr)
					} else {
						require.Error(t, guardErr)
					}

					// Start after resolution so missing/corrupt records exercise
					// the reap advice rather than the title resolver's refresh.
					_, err = manager.restoreLostOrDeadSession(repoID, inst.Title, inst, false)
					require.Error(t, err)
					require.Zero(t, backend.recoverCalls())
					requireSandboxSurvived(t, reap, "restore refused before replacement")
					want, unwanted := killSuggestionFor(inst), forceReapSuggestionFor(inst)
					if tc.wantForce {
						want, unwanted = unwanted, want
					}
					require.Contains(t, err.Error(), want)
					require.NotContains(t, err.Error(), unwanted)
					if !tc.wantForce {
						require.NotContains(t, err.Error(), "--force-reap")
					}
				})
			}
		})
	}
}

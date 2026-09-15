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
				foreignFirst bool
				missing      bool
				unreadable   bool
				wantForce    bool
				repairHint   string
			}{
				{name: "unknown_branch", storedID: "current"},
				{name: "missing_record", memoryBranch: "af/current", missing: true, repairHint: "stored record is missing"},
				{name: "unrecorded_branch", memoryBranch: "af/current", storedID: "current"},
				{name: "blank_stored_branch", memoryBranch: "af/current", storedBranch: " \t", storedID: "current"},
				{name: "reused_title", memoryBranch: "af/current", storedBranch: "af/current", storedID: "previous", repairHint: "different session"},
				{name: "stale_branch", memoryBranch: "af/current", storedBranch: "af/previous", storedID: "current"},
				{name: "record_read_error", memoryBranch: "af/current", unreadable: true, repairHint: "could not read its stored record"},
				{name: "unknown_branch_missing_record", missing: true, repairHint: "stored record is missing"},
				{name: "unknown_branch_read_error", unreadable: true, repairHint: "could not read its stored record"},
				{name: "unknown_branch_reused_title", storedID: "previous", repairHint: "different session"},
				{name: "reused_title_blank_branch", memoryBranch: "af/current", storedID: "previous", repairHint: "different session"},
				{name: "durable_matching_branch", memoryBranch: "af/current", storedBranch: "af/current", storedID: "current", wantForce: true},
				{name: "foreign_before_durable_match", memoryBranch: "af/current", storedBranch: "af/current", storedID: "current", foreignFirst: true, wantForce: true},
				{name: "foreign_before_unknown_branch_match", storedID: "current", foreignFirst: true},
				{name: "foreign_durable_before_unrecorded_match", memoryBranch: "af/current", storedID: "current", foreignFirst: true},
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
					if tc.foreignFirst {
						foreign := inst.ToInstanceData()
						foreign.ID, foreign.Branch = "previous", "af/current"
						records = append(records, foreign)
					}
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
					if tc.repairHint != "" {
						// The guard and both callers must retain repair advice. A
						// title-only kill could target a successor or fail its tombstone.
						for _, diagnostic := range []string{err.Error(), guardErr.Error()} {
							require.NotContains(t, diagnostic, "sessions kill")
							require.NotContains(t, diagnostic, "--force-reap")
							require.Contains(t, diagnostic, tc.repairHint)
							require.Contains(t, diagnostic, "Retry")
						}
						return
					}
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

// A successful archive response with no branch is another refusal, not a new
// basis for choosing an off-ramp. It must retain the advice already classified
// from the durable-record guard: otherwise it can replace executable force-reap
// advice with an unnecessarily destructive kill, or advertise a title-only kill
// while storage or identity state makes that command fail or target a successor.
func TestRestoreSession_EmptyArchiveBranchKeepsGuardDerivedAdvice(t *testing.T) {
	for _, tc := range []struct {
		name         string
		memoryBranch string
		storedBranch string
		storedID     string
		unreadable   bool
		wantForce    bool
		repairHint   string
	}{
		{name: "unknown_branch", storedID: "current"},
		{name: "unrecorded_branch", memoryBranch: "af/current", storedID: "current"},
		{name: "reused_title", memoryBranch: "af/current", storedBranch: "af/current", storedID: "previous", repairHint: "different session"},
		{name: "record_read_error", memoryBranch: "af/current", unreadable: true, repairHint: "could not read its stored record"},
		{name: "durable_matching_branch", memoryBranch: "af/current", storedBranch: "af/current", storedID: "current", wantForce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			srv := newSandboxProbeServer(t, "")
			inst, backend, reap := registerStartedRemoteWithReap(t, manager, repoID, repoPath, "empty-archive-branch", srv.url, session.Lost)
			inst.ID = "current"
			inst.SetSandboxBranch(tc.memoryBranch)
			rec := inst.ToInstanceData()
			rec.ID, rec.Branch = tc.storedID, tc.storedBranch
			raw, err := json.Marshal([]session.InstanceData{rec})
			require.NoError(t, err)
			if tc.unreadable {
				raw = []byte(`{`)
			}
			require.NoError(t, config.SaveRepoInstances(repoID, raw))

			// Start after resolution so a corrupt record reaches the refusal whose
			// guidance was classified before the archive attempt.
			_, err = manager.restoreLostOrDeadSession(repoID, inst.Title, inst, false)
			require.Error(t, err)
			require.Contains(t, err.Error(), "push reported no branch name")
			require.EqualValues(t, 1, srv.archiveCalls.Load())
			require.Zero(t, backend.recoverCalls())
			requireSandboxSurvived(t, reap, "an empty archive branch cannot license replacement")

			if tc.repairHint != "" {
				require.Contains(t, err.Error(), tc.repairHint)
				require.NotContains(t, err.Error(), "sessions kill")
				require.NotContains(t, err.Error(), "--force-reap")
				return
			}
			want, unwanted := killSuggestionFor(inst), forceReapSuggestionFor(inst)
			if tc.wantForce {
				want, unwanted = unwanted, want
			}
			require.Contains(t, err.Error(), want)
			require.NotContains(t, err.Error(), unwanted)
		})
	}
}

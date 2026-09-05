package session

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdatedAtSerializationIsReadOnly(t *testing.T) {
	at := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	i := &Instance{Title: "archived", CreatedAt: at, UpdatedAt: at, liveness: LiveArchived}
	first := i.ToInstanceData()
	second := i.ToInstanceData()
	require.Equal(t, first.UpdatedAt, second.UpdatedAt)
	require.Equal(t, at, first.UpdatedAt)
	i.GetPrompt()
	i.GetBranch()
	i.GetTabs()
	require.Equal(t, at, i.UpdatedAt)
	// Serialization must not repair zero values either; only loading does that.
	i.UpdatedAt = time.Time{}
	require.True(t, i.ToInstanceData().UpdatedAt.IsZero())
}

func TestUpdatedAtStorageRoundTrip(t *testing.T) {
	created := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	for _, updated := range []time.Time{created.Add(time.Hour), {}} {
		t.Run(updated.String(), func(t *testing.T) {
			original := &Instance{Title: "archived", CreatedAt: created, UpdatedAt: updated, liveness: LiveArchived, backend: newInertSandboxBackend("docker")}
			raw, err := json.Marshal(original.ToInstanceData().ForStorage())
			require.NoError(t, err)
			var stored InstanceData
			require.NoError(t, json.Unmarshal(raw, &stored))
			loaded, err := FromInstanceData(stored)
			require.NoError(t, err)
			want := updated
			if want.IsZero() {
				want = created
			}
			require.Equal(t, want, loaded.UpdatedAt)
			require.Equal(t, want, loaded.ToInstanceData().UpdatedAt)
		})
	}
}

func TestUpdatedAtLegacyTabReconstruction(t *testing.T) {
	at := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	for _, tabs := range [][]TabData{nil, {{ID: "agent", Kind: TabKindAgent}}} {
		i := &Instance{CreatedAt: at, UpdatedAt: at}
		restoreLocalTabs(i, InstanceData{Title: "legacy", Program: "claude", Tabs: tabs, AgentConversation: &AgentConversationData{Agent: "claude", ID: "old"}})
		require.Equal(t, at, i.UpdatedAt)
		require.Equal(t, "old", i.AgentConversation().ID)
	}
}

func TestUpdatedAtUnchangedWritesAndCaches(t *testing.T) {
	at := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return at.Add(time.Hour) }
	t.Cleanup(func() { instanceNow = oldClock })
	cases := map[string]func(*Instance){
		"prompt":           func(i *Instance) { i.SetPrompt("") },
		"title":            func(i *Instance) { require.NoError(t, i.SetTitle("same")) },
		"branch":           func(i *Instance) { i.SetSandboxBranch("") },
		"handoff mission":  func(i *Instance) { i.SetPendingHandoffMission(""); i.ClearPendingHandoffMission("stale") },
		"liveness poll":    func(i *Instance) { require.NoError(t, i.Transition(ObserveLiveness(LiveReady))) },
		"stale pane churn": func(i *Instance) { require.False(t, i.RecordPaneChurnAtEpoch(at, 99)) },
		"duplicate prompt": func(i *Instance) {
			i.lastPromptAttemptAt = at
			i.lastPromptDeliveryStatus = PromptDelivered
			require.False(t, i.RecordPromptAttempt(PromptDelivered, at))
		},
		"duplicate pane churn": func(i *Instance) { i.lastPaneChurnAt = at; require.False(t, i.RecordPaneChurnAtEpoch(at, 0)) },
		"rename unchanged":     func(i *Instance) { _, err := i.RenameTab(1, "web"); require.NoError(t, err) },
		"reorder unchanged":    func(i *Instance) { require.NoError(t, i.ReorderTab(1, 1)) },
		"rejected title":       func(i *Instance) { i.started = true; require.Error(t, i.SetTitle("different")) },
		"empty evidence":       func(i *Instance) { i.ClearIdleEvidence() },
		"root snapshot":        func(i *Instance) { i.ReconcileRootRecreateContext(RootRecreateContextNone) },
		"failed tab checkpoint": func(i *Instance) {
			_, err := i.CloseTabByIDWithCommit("web", func(InstanceData) error { return fmt.Errorf("disk unavailable") })
			require.Error(t, err)
			require.Len(t, i.Tabs, 2)
		},
		"PR cache": func(i *Instance) {
			i.SetPRInfo(nil)
			i.MarkPRInfoFetched()
			claim, _ := i.BeginPRInfoFetch(0)
			i.CancelPRInfoFetch(claim)
		},
		"PR cache rollback": func(i *Instance) {
			rollback := i.BeginPRInfoWrite(nil)
			i.RollbackPRInfoWrite(rollback)
			i.SetPRInfoFetchedAtForTest(at)
		},
		"derived diagnostics": func(i *Instance) {
			i.SetAgentModelChangeAtEpoch(NewAgentModelChange("a", "b"), 0)
			i.ClearAgentModelChange()
			i.ReconcileAgentModelChange(nil)
			i.ReconcileArchiveWarning("warning")
		},
		"server cache": func(i *Instance) { i.AgentServer(); i.agentObservationTarget() },
		"storage identity": func(i *Instance) {
			i.PinStorageRepoID("remembered")
			require.Equal(t, "remembered", i.repoIDForStorage())
		},
		"load coordination": func(i *Instance) { i.markLoadRuntimeReplaced(); i.ConsumeLoadRuntimeReplacement() },
		"adoption fence":    func(i *Instance) { i.CloseAdoptionFence(); i.ReopenAdoptionFence() },
		"credential wiring": func(i *Instance) { i.SetSandboxCredentials(nil); SetRuntimeTeardownForTest(i, nil) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			i := &Instance{Title: "same", UpdatedAt: at, liveness: LiveReady, Tabs: []*Tab{{Kind: TabKindAgent}, {ID: "web", Name: "web", Kind: TabKindWeb}}}
			mutate(i)
			require.Equal(t, at, i.UpdatedAt)
			require.Equal(t, at, i.ToInstanceData().UpdatedAt)
		})
	}
}

func TestUpdatedAtStorageLoadAndSave(t *testing.T) {
	created := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	for _, status := range []Status{Archived, Lost} {
		for _, updated := range []time.Time{created.Add(time.Hour), {}} {
			t.Run(fmt.Sprintf("%d/%s", status, updated), func(t *testing.T) {
				root := t.TempDir()
				record := InstanceData{ID: "stored", Title: "stored", Path: root, Status: status, Program: "claude", CreatedAt: created, UpdatedAt: updated,
					Worktree: GitWorktreeData{RepoPath: root, WorktreePath: root, SessionName: "stored", BranchName: "main", ExternalWorktree: true}}
				raw, err := json.Marshal([]InstanceData{record})
				require.NoError(t, err)
				ms := newMockStorage()
				ms.data["stored-repo"] = raw
				storage, err := NewStorage(ms, "")
				require.NoError(t, err)
				loaded, err := storage.LoadInstances()
				require.NoError(t, err)
				require.Len(t, loaded, 1)
				want := updated
				if want.IsZero() {
					want = created
				}
				require.Equal(t, want, loaded[0].UpdatedAt)
				require.NoError(t, storage.SaveInstances(loaded))
				var saved []InstanceData
				require.NoError(t, json.Unmarshal(ms.data["stored-repo"], &saved))
				require.Len(t, saved, 1)
				require.Equal(t, want, saved[0].UpdatedAt)
			})
		}
	}
}

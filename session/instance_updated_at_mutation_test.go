package session

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestUpdatedAtMutations(t *testing.T) {
	clock := time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	previous := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = previous })
	local := func(t *testing.T, i *Instance) {
		i.started = true
		i.gitWorktree = &git.GitWorktree{}
		i.Tabs[0].tmux = tmux.NewTmuxSession("updated-at", "claude")
	}
	cases := []struct {
		name    string
		prepare func(*testing.T, *Instance)
		mutate  func(*testing.T, *Instance)
	}{
		{"prompt", nil, func(t *testing.T, i *Instance) { i.SetPrompt("new goal") }},
		{"handoff mission", nil, func(t *testing.T, i *Instance) { i.SetPendingHandoffMission("brief") }},
		{"clear handoff mission", func(t *testing.T, i *Instance) { i.pendingHandoffMission = "brief" }, func(t *testing.T, i *Instance) { i.ClearPendingHandoffMission("brief") }},
		{"title", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.SetTitle("renamed")) }},
		{"branch", nil, func(t *testing.T, i *Instance) { i.SetSandboxBranch("topic") }},
		{"tombstone", nil, func(t *testing.T, i *Instance) { i.MarkUserKilled() }},
		{"tombstone snapshot", nil, func(t *testing.T, i *Instance) { i.ReconcileUserKilledSnapshot(true) }},
		{"startup unknown", nil, func(t *testing.T, i *Instance) { i.MarkStartupStateUnknown() }},
		{"status", nil, func(t *testing.T, i *Instance) { i.SetStatusForTest(Running) }},
		{"operation", nil, func(t *testing.T, i *Instance) { i.SetInFlightOpForTest(OpCreating) }},
		{"started", nil, func(t *testing.T, i *Instance) { i.SetStartedForTest(true) }},
		{"archive", nil, func(t *testing.T, i *Instance) { i.SetArchived() }},
		{"restore", func(t *testing.T, i *Instance) { i.liveness = LiveArchived }, func(t *testing.T, i *Instance) { require.NoError(t, i.Transition(BeginRestore())) }},
		{"liveness", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.Transition(ObserveLiveness(LiveRunning))) }},
		{"limit", nil, func(t *testing.T, i *Instance) { i.SetLimitReached(clock.Add(time.Hour)) }},
		{"limit at epoch", nil, func(t *testing.T, i *Instance) { i.SetLimitReachedAtEpoch(clock.Add(time.Hour), i.stateEpoch) }},
		{"limit reset", nil, func(t *testing.T, i *Instance) { i.SetLimitResetAt(clock.Add(time.Hour)) }},
		{"clear limit", func(t *testing.T, i *Instance) { i.liveness = LiveLimitReached }, func(t *testing.T, i *Instance) { i.ClearLimitReached() }},
		{"repark limit", func(t *testing.T, i *Instance) { i.inFlightOp = OpRespawning }, func(t *testing.T, i *Instance) { require.NoError(t, i.ReparkLimitUnderResumeFence(clock)) }},
		{"prompt attempt", nil, func(t *testing.T, i *Instance) { i.RecordPromptAttempt(PromptDelivered, clock) }},
		{"pane churn", nil, func(t *testing.T, i *Instance) { i.RecordPaneChurnAtEpoch(clock, i.stateEpoch) }},
		{"pane churn checkpoint", nil, func(t *testing.T, i *Instance) { i.RecordPaneChurnCheckpointAtEpoch(clock, i.stateEpoch) }},
		{"clear idle evidence", func(t *testing.T, i *Instance) { i.lastPromptAttemptAt = clock }, func(t *testing.T, i *Instance) { i.ClearIdleEvidence() }},
		{"idle snapshot", nil, func(t *testing.T, i *Instance) { i.ReconcileIdleEvidence(clock, PromptDelivered, clock) }},
		{"restore failure", nil, func(t *testing.T, i *Instance) { i.SetLostRestoreFailure(3, errors.New("gone")) }},
		{"clear restore failure", func(t *testing.T, i *Instance) { i.lostRestoreFailure = LostRestoreFailure{Attempts: 3, Error: "gone"} }, func(t *testing.T, i *Instance) { i.ClearLostRestoreFailure() }},
		{"restore failure snapshot", nil, func(t *testing.T, i *Instance) {
			i.ReconcileLostRestoreFailure(&LostRestoreFailure{Attempts: 3, Error: "gone"})
		}},
		{"root context", nil, func(t *testing.T, i *Instance) { i.ReconcileRootRecreateContext(RootRecreateContextFresh) }},
		{"root acknowledgement", func(t *testing.T, i *Instance) { i.rootRecreateContext = RootRecreateContextFresh }, func(t *testing.T, i *Instance) { i.AcknowledgeRootRecreateContext() }},
		{"conversation", nil, func(t *testing.T, i *Instance) {
			i.SetAgentConversation(AgentConversationData{Agent: "claude", ID: "new"})
		}},
		{"conversation runtime", nil, func(t *testing.T, i *Instance) {
			i.SetAgentConversationForRuntime(i.AgentRuntimeToken(), AgentConversationData{Agent: "claude", ID: "new"})
		}},
		{"account selection", func(t *testing.T, i *Instance) { i.inFlightOp = OpRespawning }, func(t *testing.T, i *Instance) {
			_, err := i.SelectAccountAutomatically("", "work")
			require.NoError(t, err)
		}},
		{"account rollback", func(t *testing.T, i *Instance) { i.inFlightOp = OpRespawning; i.Account = "work" }, func(t *testing.T, i *Instance) {
			require.NoError(t, i.RestoreAccountSelectionUnderResumeFence("personal", false, AgentConversationData{}))
		}},
		{"clear account swap", func(t *testing.T, i *Instance) { i.pendingAccountSwap = &AccountSwapData{From: "old", To: "new"} }, func(t *testing.T, i *Instance) { i.ClearPendingAccountSwap("old", "new") }},
		{"account panes", func(t *testing.T, i *Instance) { i.pendingAccountSwap = &AccountSwapData{} }, func(t *testing.T, i *Instance) { require.NoError(t, i.markAccountSwapReplacementPanesStarted()) }},
		{"add tab", nil, func(t *testing.T, i *Instance) { i.AddTabForTest("extra", TabKindShell) }},
		{"add web tab fixture", nil, func(t *testing.T, i *Instance) { i.AddWebTabForTest("extra", "https://example.com") }},
		{"rename tab", nil, func(t *testing.T, i *Instance) { _, err := i.RenameTab(1, "renamed"); require.NoError(t, err) }},
		{"rename tab by id", nil, func(t *testing.T, i *Instance) { _, err := i.RenameTabByID("web1", "renamed"); require.NoError(t, err) }},
		{"reorder tab", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.ReorderTab(1, 2)) }},
		{"reorder tab by id", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.ReorderTabByID("web1", 2)) }},
		{"close tab", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.CloseTab(1)) }},
		{"close tab by id", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.CloseTabByID("web1")) }},
		{"drop tab", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.DropClosedTab(1)) }},
		{"cleanup roster", nil, func(t *testing.T, i *Instance) {
			i.SetPendingTabCleanupForTest([]TabCleanupData{{TabID: "tab", TmuxName: "pending"}})
		}},
		{"runtime cleanup unknown", nil, func(t *testing.T, i *Instance) { i.markRuntimeCleanupStateUnknown() }},
		{"adoption delivery", nil, func(t *testing.T, i *Instance) { require.NoError(t, i.NoteAdoptionDelivery()) }},
		{"runtime replacement", nil, func(t *testing.T, i *Instance) { i.noteAgentRuntimeReplaced() }},
		{"backend", nil, func(t *testing.T, i *Instance) { i.SetBackend(&LocalBackend{}) }},
		{"worktree fixture", nil, func(t *testing.T, i *Instance) { i.SetGitWorktreeForTest(&git.GitWorktree{}) }},
		{"tmux binding", nil, func(t *testing.T, i *Instance) { i.SetTmuxSession(tmux.NewTmuxSession("updated-at", "claude")) }},
		{"root note", func(t *testing.T, i *Instance) { i.carriedRecreateNotice = RootRecreateContextFresh }, func(t *testing.T, i *Instance) { i.NoteRecreateContext() }},
		{"root refresh", func(t *testing.T, i *Instance) {
			i.rootRecreateContext = RootRecreateContextUnknown
			i.carriedRecreateNotice = RootRecreateContextFresh
		}, func(t *testing.T, i *Instance) { i.RefreshRecreateContext() }},
		{"restore failure observation", func(t *testing.T, i *Instance) {
			i.lostRestoreFailure = LostRestoreFailure{Attempts: 3, Error: "gone"}
			i.agentObservationGeneration.Store(1)
		}, func(t *testing.T, i *Instance) {
			i.ClearLostRestoreFailureAtObservation(AgentObservationGeneration{value: 1})
		}},
		{"prompt observation", nil, func(t *testing.T, i *Instance) { i.recordPromptAttemptForObservation(PromptDelivered, clock, nil) }},
		{"program swap", func(t *testing.T, i *Instance) { i.started = true }, func(t *testing.T, i *Instance) {
			_, err := i.SwapAgentProgram("codex", "switch", "", false)
			require.NoError(t, err)
		}},
		{"program swap under fence", func(t *testing.T, i *Instance) { i.inFlightOp = OpReplacing }, func(t *testing.T, i *Instance) {
			_, err := i.RecordHandoffSwap("codex", "switch", "", false)
			require.NoError(t, err)
		}},
		{"revert program", func(t *testing.T, i *Instance) {
			i.Program = "codex"
			i.Tabs[0].Handoffs = []AgentHandoff{{To: "codex"}}
		}, func(t *testing.T, i *Instance) {
			require.NoError(t, i.RevertHandoff(HandoffSwap{AgentHandoff: AgentHandoff{To: "codex"}, previousProgram: "claude"}))
		}},
		{"add web tab", func(t *testing.T, i *Instance) { i.started = true }, func(t *testing.T, i *Instance) {
			_, err := i.AddWebTab("https://example.com", "extra")
			require.NoError(t, err)
		}},
		{"add vscode tab", local, func(t *testing.T, i *Instance) { _, err := i.AddVSCodeTab("editor"); require.NoError(t, err) }},
		{"attach vscode tab", local, func(t *testing.T, i *Instance) {
			_, err := i.AttachVSCodeTab("editor", "vscode")
			require.NoError(t, err)
		}},
		{"tab snapshot", local, func(t *testing.T, i *Instance) {
			i.ReconcileTabsFromData([]TabData{{ID: "agent", Kind: TabKindAgent}, {ID: "web1", Name: "changed", Kind: TabKindWeb}, {ID: "web2", Name: "two", Kind: TabKindWeb}})
		}},
		{"tab order snapshot", nil, func(t *testing.T, i *Instance) {
			i.reorderTabsFromData([]TabData{{Name: "two", Kind: TabKindWeb}, {Name: "one", Kind: TabKindWeb}})
		}},
		{"drop tab by name", nil, func(t *testing.T, i *Instance) { i.dropTabByName("one") }},
		{"drop tab by id", nil, func(t *testing.T, i *Instance) { i.dropTabByID("web1") }},
		{"committed tab close", nil, func(t *testing.T, i *Instance) {
			_, err := i.CloseTabByIDWithCommit("web1", func(data InstanceData) error { require.Equal(t, clock, data.UpdatedAt); return nil })
			require.NoError(t, err)
		}},
		{"pending metadata tabs", func(t *testing.T, i *Instance) {
			i.pendingMetadataTabs = []TabData{{ID: "new", Name: "new", Kind: TabKindWeb}}
		}, func(t *testing.T, i *Instance) { i.mu.Lock(); defer i.mu.Unlock(); i.appendPendingMetadataTabsLocked() }},
		{"runtime reset", func(t *testing.T, i *Instance) { i.runtimeCleanupStateUnknown = true }, func(t *testing.T, i *Instance) { i.resetRemoteRuntime() }},
		{"retain runtime cleanup", nil, func(t *testing.T, i *Instance) {
			i.retainProvisionResultCleanup(ProvisionResult{Backend: newInertSandboxBackend("docker")})
		}},
		{"teardown fixture", nil, func(t *testing.T, i *Instance) { SetRuntimeTeardownForTest(i, func() error { return nil }) }},
		{"retained account quota", func(t *testing.T, i *Instance) {
			i.Account = "work"
			i.liveness = LiveLimitReached
			i.limitResetAt = clock.Add(time.Hour)
			i.limitAccount = "work"
		}, func(t *testing.T, i *Instance) { i.SetLimitReached(i.limitResetAt) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instance{Title: "session", Program: "claude", liveness: LiveReady, CreatedAt: clock, UpdatedAt: clock,
				Tabs: []*Tab{{ID: "agent", Kind: TabKindAgent}, {ID: "web1", Name: "one", Kind: TabKindWeb}, {ID: "web2", Name: "two", Kind: TabKindWeb}}}
			if tc.prepare != nil {
				tc.prepare(t, i)
			}
			before := i.UpdatedAt
			clock = clock.Add(time.Minute)
			tc.mutate(t, i)
			require.Equal(t, clock, i.UpdatedAt)
			require.True(t, i.UpdatedAt.After(before))
			require.Equal(t, clock, i.ToInstanceData().UpdatedAt)
		})
	}
}

package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// A conversation identity a remote agent-server reports AFTER launch, while the
// session stays Running, is written by the very next poll persistence — not
// only on a liveness or reset change (#5826). Without it the resume handle
// lived in memory and a daemon restart lost it.
func TestPollPersistsALateReportedConversation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, err := session.NewInstance(session.InstanceOptions{Title: "late", Path: repoPath, Program: "codex"})
	require.NoError(t, err)
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Ready)
	inst.Tabs = append(inst.Tabs, &session.Tab{ID: "agent", Name: "agent", Kind: session.TabKindAgent})
	seedDiskInstance(t, repoID, "late", repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, "late")] = inst
	manager.mu.Unlock()

	persistedConversation := func() *session.AgentConversationData {
		raw, err := config.LoadRepoInstances(repoID)
		require.NoError(t, err)
		var rows []session.InstanceData
		require.NoError(t, json.Unmarshal(raw, &rows))
		for _, d := range rows {
			if d.Title == "late" {
				return d.AgentConversation
			}
		}
		return nil
	}
	require.Nil(t, persistedConversation(), "anti-vacuous: nothing persisted yet")

	conv := session.AgentConversationData{Agent: "codex", ID: "019a3333-aaaa-bbbb-cccc-ddddeeeeffff", CaptureKind: session.ConversationCaptureCodexRollout}
	require.True(t, inst.RecordReportedConversationForTest(conv))
	require.True(t, inst.ConversationUnpersisted())

	// No liveness change, no reset change, no checkpoint: only the report.
	manager.persistPollChangeWithIdleEvidence(repoID, inst, inst.GetLiveness(), inst.ToInstanceData().LimitResetAt, false, false)

	got := persistedConversation()
	require.NotNil(t, got, "the reported identity must be durable after the next poll")
	assert.Equal(t, conv.ID, got.ID)
	assert.False(t, inst.ConversationUnpersisted(), "a successful write clears the pending flag")
}

package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #2629 suite. #2616 made a re-created root carry its conversation and made
// the three outcomes legible IN THE LOG. What it left unbuilt is the
// user-facing half: a root that genuinely could not resume still comes back
// Ready, with a rail row identical to one that resumed cleanly, and you learn
// otherwise from a log grep.

const priorConversationID = "64ea06ed-7206-7fc2-803b-f7045e07a242"

// TestClassifyRootRecreateContext pins the judgment every surface reads. The
// middle case is the one worth staring at: a replacement that recorded NO
// conversation may have continued on its own (`codex resume --last`), so af
// must not call it a fresh context — it does not know.
func TestClassifyRootRecreateContext(t *testing.T) {
	prior := AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID}

	tests := []struct {
		name    string
		carried AgentConversationData
		created *AgentConversationData
		want    RootRecreateContext
		note    string
	}{
		{
			name:    "resumed the exact conversation",
			carried: prior,
			created: &AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID},
			want:    RootRecreateContextNone,
			note:    "",
		},
		{
			name:    "nothing was recorded to carry",
			carried: AgentConversationData{},
			created: nil,
			want:    RootRecreateContextFresh,
			note:    "fresh context",
		},
		{
			name:    "came up on a different conversation",
			carried: prior,
			created: &AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"},
			want:    RootRecreateContextFresh,
			note:    "fresh context",
		},
		{
			name:    "the resolved command selects its own conversation",
			carried: prior,
			created: nil,
			want:    RootRecreateContextUnknown,
			note:    "context unknown",
		},
		{
			name:    "a different agent's id is not the same conversation",
			carried: prior,
			created: &AgentConversationData{Agent: tmux.ProgramCodex, ID: priorConversationID},
			want:    RootRecreateContextFresh,
			note:    "fresh context",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRootRecreateContext(tc.carried, tc.created)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.note, got.Note())
		})
	}
}

// TestRootRecreateNoteIgnoresUnknownValues: the note is rendered from a
// persisted string, so a value a newer daemon writes must render NOTHING rather
// than leak a raw enum onto the rail or be guessed at.
func TestRootRecreateNoteIgnoresUnknownValues(t *testing.T) {
	assert.Equal(t, "", RootRecreateContext("something-later").Note())
	assert.Equal(t, "", RootRecreateContextNone.Note())
}

// TestNoteRecreateContextReadsTheCommittedConversation: the marker is decided
// from what the record CARRIES, not from what the create was asked to resume. A
// create can be handed a conversation and still come up on another one, and the
// record is what a future recovery resumes from.
func TestNoteRecreateContextReadsTheCommittedConversation(t *testing.T) {
	prior := AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID}

	t.Run("resumed", func(t *testing.T) {
		inst := &Instance{carriedConversation: prior, Tabs: []*Tab{{Name: agentTabName, Conversation: prior}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext())
	})
	t.Run("landed on a different conversation", func(t *testing.T) {
		fresh := AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"}
		inst := &Instance{carriedConversation: prior, Tabs: []*Tab{{Name: agentTabName, Conversation: fresh}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	})
	t.Run("recorded nothing", func(t *testing.T) {
		inst := &Instance{carriedConversation: prior, Tabs: []*Tab{{Name: agentTabName}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextUnknown, inst.RootRecreateContext())
	})
	t.Run("had nothing to carry", func(t *testing.T) {
		inst := &Instance{Tabs: []*Tab{{Name: agentTabName}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	})
}

// TestAcknowledgeRootRecreateContextIsOneShot: exactly one acknowledger reports
// the change, so two clients opening the pane at once cannot both persist and
// re-announce the same cleared row.
func TestAcknowledgeRootRecreateContextIsOneShot(t *testing.T) {
	inst := &Instance{rootRecreateContext: RootRecreateContextFresh}
	require.True(t, inst.AcknowledgeRootRecreateContext())
	assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext())
	assert.False(t, inst.AcknowledgeRootRecreateContext(),
		"a second acknowledgement has nothing to clear and must not report a change")

	unmarked := &Instance{}
	assert.False(t, unmarked.AcknowledgeRootRecreateContext(),
		"an ordinary session is never acknowledging anything")
}

// TestRootRecreateContextSurvivesStorageRoundTrip is the load-bearing
// persistence assertion. The marker must NOT be scrubbed by ForStorage the way
// the projection-only diagnostics beside it are: a daemon restart is a likely
// part of the very outage that produced the note, so scrubbing it would erase
// the notice exactly when it matters.
func TestRootRecreateContextSurvivesStorageRoundTrip(t *testing.T) {
	inst := &Instance{
		ID:      "root-2629",
		Title:   "root",
		Program: "claude",
		// Lost so the reload below stays inert — FromInstanceData returns before
		// any tmux re-spawn for a Lost record (#970). What is under test is the
		// field's durability, not a relaunch.
		liveness:            LiveLost,
		rootRecreateContext: RootRecreateContextFresh,
	}
	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: t.TempDir(), WorktreePath: t.TempDir(), SessionName: "root", BranchName: "main",
	}
	require.Equal(t, RootRecreateContextFresh, stored.RootRecreateContext,
		"ForStorage must keep the note: it is durable state, not a live projection")

	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"root_recreate_context":"fresh"`)

	var decoded InstanceData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	rebuilt, err := FromInstanceData(decoded)
	require.NoError(t, err)
	assert.Equal(t, RootRecreateContextFresh, rebuilt.RootRecreateContext(),
		"a daemon restart must not lose the notice the outage produced")
}

// TestRootRecreateContextIsOmittedForOrdinarySessions: every session ever
// written carries this field, so it must stay out of the JSON unless there is
// something to say.
func TestRootRecreateContextIsOmittedForOrdinarySessions(t *testing.T) {
	inst := &Instance{ID: "plain-2629", Title: "plain", liveness: LiveReady}
	raw, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "root_recreate_context")
}

// TestReconcileRootRecreateContextAppliesBothDirections: a client's projection
// must adopt the note AND drop it. A monotonic reconcile — the shape the kill
// tombstone correctly uses — would burn the note onto every other open rail
// after the user had already read the pane.
func TestReconcileRootRecreateContextAppliesBothDirections(t *testing.T) {
	inst := &Instance{}
	require.True(t, inst.ReconcileRootRecreateContext(RootRecreateContextFresh))
	assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	assert.False(t, inst.ReconcileRootRecreateContext(RootRecreateContextFresh),
		"an unchanged value is not a change")
	require.True(t, inst.ReconcileRootRecreateContext(RootRecreateContextNone))
	assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext())
}

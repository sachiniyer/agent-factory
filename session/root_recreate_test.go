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
		name     string
		carried  AgentConversationData
		created  *AgentConversationData
		launched string
		want     RootRecreateContext
		note     string
	}{
		{
			name:     "resumed the exact conversation",
			carried:  prior,
			created:  &AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID},
			launched: tmux.ProgramClaude,
			want:     RootRecreateContextNone,
			note:     "",
		},
		{
			name:     "nothing was recorded to carry",
			carried:  AgentConversationData{},
			created:  nil,
			launched: tmux.ProgramClaude,
			want:     RootRecreateContextFresh,
			note:     "fresh context",
		},
		{
			name:     "came up on a different conversation",
			carried:  prior,
			created:  &AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"},
			launched: tmux.ProgramClaude,
			want:     RootRecreateContextFresh,
			note:     "fresh context",
		},
		{
			// The SAME agent recorded nothing, which happens only when the resolved
			// command pins its own conversation selection.
			name:     "the resolved command selects its own conversation",
			carried:  prior,
			created:  nil,
			launched: tmux.ProgramClaude,
			want:     RootRecreateContextUnknown,
			note:     "context unknown",
		},
		{
			// The #2814 Codex P2. A claude root repointed to codex records nothing
			// synchronously (codex ids are discovered asynchronously), but the claude
			// conversation is PROVABLY not resumed — a codex process cannot be in it.
			// Reading this as "unknown" hid the documented agent-change fallback
			// behind the one word that means af cannot tell.
			name:     "the root now runs a different agent",
			carried:  prior,
			created:  nil,
			launched: tmux.ProgramCodex,
			want:     RootRecreateContextFresh,
			note:     "fresh context",
		},
		{
			// An unidentifiable launch must not EARN the unknown verdict: it is the
			// answer to "same provider?", and that question was not answered.
			name:     "the launched agent cannot be identified",
			carried:  prior,
			created:  nil,
			launched: "",
			want:     RootRecreateContextFresh,
			note:     "fresh context",
		},
		{
			name:     "a different agent's id is not the same conversation",
			carried:  prior,
			created:  &AgentConversationData{Agent: tmux.ProgramCodex, ID: priorConversationID},
			launched: tmux.ProgramCodex,
			want:     RootRecreateContextFresh,
			note:     "fresh context",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRootRecreateContext(tc.carried, tc.created, tc.launched)
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
		inst := &Instance{Program: tmux.ProgramClaude, carriedConversation: prior,
			Tabs: []*Tab{{Name: agentTabName, Conversation: prior}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext())
	})
	t.Run("landed on a different conversation", func(t *testing.T) {
		fresh := AgentConversationData{Agent: tmux.ProgramClaude, ID: "5299e00d-1111-4222-8333-f7045e07a242"}
		inst := &Instance{Program: tmux.ProgramClaude, carriedConversation: prior,
			Tabs: []*Tab{{Name: agentTabName, Conversation: fresh}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	})
	t.Run("same agent recorded nothing", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramClaude, carriedConversation: prior,
			Tabs: []*Tab{{Name: agentTabName}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextUnknown, inst.RootRecreateContext())
	})
	t.Run("the root now runs a different agent", func(t *testing.T) {
		// The repointed-program fallback: the launch resolves to codex, whose ids
		// are captured asynchronously, so nothing is recorded here — but a codex
		// process is provably not in the carried claude conversation.
		inst := &Instance{Program: tmux.ProgramCodex, carriedConversation: prior,
			Tabs: []*Tab{{Name: agentTabName}}}
		inst.NoteRecreateContext()
		assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	})
	t.Run("had nothing to carry", func(t *testing.T) {
		inst := &Instance{Program: tmux.ProgramClaude, Tabs: []*Tab{{Name: agentTabName}}}
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

// TestAcknowledgeRootRecreateContextPreservesUnknownValues is the #2814 Codex
// P2: a record written by a NEWER daemon can carry an outcome this binary does
// not render. Nobody was shown it, so nobody acknowledged it — and clearing it
// on a stream open would let an old binary silently erase roll-forward state
// the newer daemon was going to display. The binary that understands a value is
// the one that gets to clear it.
func TestAcknowledgeRootRecreateContextPreservesUnknownValues(t *testing.T) {
	future := RootRecreateContext("some-later-outcome")
	require.Empty(t, future.Note(), "fixture guard: this value must be one this binary does not render")

	inst := &Instance{rootRecreateContext: future}
	assert.False(t, inst.AcknowledgeRootRecreateContext(),
		"a value this binary never rendered has not been acknowledged")
	assert.Equal(t, future, inst.RootRecreateContext(),
		"the newer daemon's state must survive an older binary's stream opens")
}

// TestPendingNoticeSurvivesASecondHeal is the #2814 Codex P2. A root can lose
// its history and then have tmux die AGAIN before anyone opens its pane. The
// second heal classifies the second replacement — which may well have resumed
// cleanly — and answers a different question than the pending warning asked. If
// that clean verdict overwrote the warning, the original loss would go back out
// of sight, unseen and unacknowledged: the exact bug this feature exists to end.
func TestPendingNoticeSurvivesASecondHeal(t *testing.T) {
	current := AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID}

	// The second heal resumes the conversation the FIRST replacement came up on,
	// so on its own inputs it is a clean carry-over.
	inst := &Instance{
		Program:               tmux.ProgramClaude,
		carriedConversation:   current,
		carriedRecreateNotice: RootRecreateContextFresh,
		Tabs:                  []*Tab{{Name: agentTabName, Conversation: current}},
	}
	require.Equal(t, RootRecreateContextNone,
		ClassifyRootRecreateContext(current, &current, tmux.ProgramClaude),
		"fixture guard: this heal must classify clean on its own, or the test proves nothing")

	inst.NoteRecreateContext()
	assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext(),
		"a clean second heal must not erase an unacknowledged warning about the first")
}

// TestASecondHealCanOnlyEscalateAPendingNotice: the floor is one-directional.
// An inherited "unknown" must be upgraded by a second heal that proves a loss,
// and never the other way round.
func TestASecondHealCanOnlyEscalateAPendingNotice(t *testing.T) {
	prior := AgentConversationData{Agent: tmux.ProgramClaude, ID: priorConversationID}

	escalates := &Instance{
		Program:               tmux.ProgramCodex,
		carriedConversation:   prior,
		carriedRecreateNotice: RootRecreateContextUnknown,
		Tabs:                  []*Tab{{Name: agentTabName}},
	}
	escalates.NoteRecreateContext()
	assert.Equal(t, RootRecreateContextFresh, escalates.RootRecreateContext(),
		"a proven loss outranks an inherited unproven one")

	holds := &Instance{
		Program:               tmux.ProgramClaude,
		carriedConversation:   prior,
		carriedRecreateNotice: RootRecreateContextFresh,
		Tabs:                  []*Tab{{Name: agentTabName}},
	}
	holds.NoteRecreateContext()
	assert.Equal(t, RootRecreateContextFresh, holds.RootRecreateContext(),
		"an inherited proven loss is not downgraded to unknown")
}

// TestRefreshRecreateContextResolvesUnknownOnceCaptureProvesContinuity is the
// other #2814 Codex P2. A root whose command selects its own conversation
// records nothing at launch, so the heal can only say "unknown". When the async
// provider capture later commits the id the agent actually came up on, and that
// id is the carried one, af has proven the continuity it could not see — and a
// warning that stays up after that is a stale warning.
func TestRefreshRecreateContextResolvesUnknownOnceCaptureProvesContinuity(t *testing.T) {
	prior := AgentConversationData{Agent: tmux.ProgramCodex, ID: priorConversationID}
	inst := &Instance{
		Program:             tmux.ProgramCodex,
		carriedConversation: prior,
		Tabs:                []*Tab{{Name: agentTabName}},
	}
	inst.NoteRecreateContext()
	require.Equal(t, RootRecreateContextUnknown, inst.RootRecreateContext(),
		"fixture guard: a pinned-resume launch records nothing, so the verdict starts unknown")

	// The capture goroutine commits the id the agent really came up on.
	inst.Tabs[0].Conversation = prior
	assert.True(t, inst.RefreshRecreateContext(), "the verdict changed and the caller must persist it")
	assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext(),
		"af has now proven the root resumed; the warning must come down")
}

// TestRefreshRecreateContextNeverResurrectsAnAcknowledgedNotice: acknowledgement
// is final. Late evidence about a launch nobody is being warned about must not
// put a notice back on a row the user already dealt with.
func TestRefreshRecreateContextNeverResurrectsAnAcknowledgedNotice(t *testing.T) {
	inst := &Instance{Program: tmux.ProgramCodex, Tabs: []*Tab{{Name: agentTabName}}}
	inst.NoteRecreateContext()
	require.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
	require.True(t, inst.AcknowledgeRootRecreateContext())

	assert.False(t, inst.RefreshRecreateContext(), "an acknowledged notice is done")
	assert.Equal(t, RootRecreateContextNone, inst.RootRecreateContext())
}

// TestRefreshRecreateContextHoldsAnInheritedNotice: late evidence about THIS
// launch cannot clear a warning inherited from an earlier heal — that warning
// is about a loss this capture says nothing about.
func TestRefreshRecreateContextHoldsAnInheritedNotice(t *testing.T) {
	prior := AgentConversationData{Agent: tmux.ProgramCodex, ID: priorConversationID}
	inst := &Instance{
		Program:               tmux.ProgramCodex,
		carriedConversation:   prior,
		carriedRecreateNotice: RootRecreateContextFresh,
		Tabs:                  []*Tab{{Name: agentTabName}},
	}
	inst.NoteRecreateContext()
	require.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())

	inst.Tabs[0].Conversation = prior
	assert.False(t, inst.RefreshRecreateContext(),
		"proving THIS launch resumed says nothing about the earlier loss")
	assert.Equal(t, RootRecreateContextFresh, inst.RootRecreateContext())
}

// TestASecondHealPreservesAnUnreadableCarriedNotice is the third-round #2814
// Codex P2, and the sibling of the acknowledge-path rule: an older binary must
// not destroy a newer daemon's pending outcome. severity() has to score an
// unrecognized value as 0 so it can never outrank a note this binary would
// actually render — but that score would otherwise let a locally-classified
// verdict overwrite it on the next heal, losing roll-forward state nobody
// acknowledged.
func TestASecondHealPreservesAnUnreadableCarriedNotice(t *testing.T) {
	future := RootRecreateContext("some-later-outcome")
	require.Empty(t, future.Note(), "fixture guard: this value must be one this binary cannot render")

	// A heal this binary classifies as a provable loss — the strongest verdict it
	// can reach, so if anything could overwrite the carried value, this would.
	inst := &Instance{
		Program:               tmux.ProgramClaude,
		carriedRecreateNotice: future,
		Tabs:                  []*Tab{{Name: agentTabName}},
	}
	inst.NoteRecreateContext()
	assert.Equal(t, future, inst.RootRecreateContext(),
		"a value this binary cannot read must not be replaced by one it computed")
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

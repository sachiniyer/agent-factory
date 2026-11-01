package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const carriedOldAccountID = "019f386f-7206-7fc2-803b-f7045e07a242"

// #6189: an id carried from a previous record names a conversation in ONE
// account's store. After an account swap, a respawn must not resume it.
func TestRespawnAfterAnAccountSwapDoesNotResumeTheOldAccountsConversation(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, "codex", "codex")
	conv := AgentConversationData{Agent: "codex", ID: carriedOldAccountID}
	inst.carriedConversation = conv
	inst.reportedConversation = conv
	inst.Tabs[0].Conversation = conv

	require.Contains(t, prepareResumeConversation(inst, "codex"), "resume "+carriedOldAccountID,
		"premise: before the swap the exact id is used")

	inst.mu.Lock()
	_, err := inst.selectAccountLocked("", "work", "codex", false)
	inst.mu.Unlock()
	require.NoError(t, err)

	assert.Equal(t, "codex", prepareResumeConversation(inst, "codex"),
		"the previous account's id must not be named once the account changed")
	assert.False(t, inst.carriedConversation.HasID())
	assert.False(t, inst.reportedConversation.HasID())
}

// The same respawn without a swap still resumes exactly, from the tab or, on a
// fresh in-sandbox instance with no tab yet, from the pinned carry (#6173).
func TestRespawnWithoutASwapStillResumesExactly(t *testing.T) {
	conv := AgentConversationData{Agent: "codex", ID: carriedOldAccountID}

	pinned := &Instance{Title: "t", carriedConversation: conv}
	assert.Contains(t, prepareResumeConversation(pinned, "codex"), "resume "+carriedOldAccountID)

	tabbed := &Instance{Title: "t", Tabs: []*Tab{{Conversation: conv}}, carriedConversation: conv}
	assert.Contains(t, prepareResumeConversation(tabbed, "codex"), "resume "+carriedOldAccountID)
	assert.Equal(t, conv, tabbed.carriedConversation, "a launch does not consume the carry: root-recreate classification reads it afterwards")
}

func TestOffBoxAccountRecordRetiresTheCarriedConversation(t *testing.T) {
	conv := AgentConversationData{Agent: "codex", ID: carriedOldAccountID}
	inst := &Instance{Title: "t", Tabs: []*Tab{{Conversation: conv}}, carriedConversation: conv, reportedConversation: conv}
	inst.recordOffBoxAccount("work", "codex", false, "codex", nil)
	assert.Equal(t, "codex", prepareResumeConversation(inst, "codex"))
}

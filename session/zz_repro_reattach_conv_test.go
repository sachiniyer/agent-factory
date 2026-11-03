package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reproduction tests for the reattach conversation-handle drop (#6162 + #5826
// interaction). A reloaded ssh/sandbox row holds its recorded conversation in
// reportedConversation; ReattachSandbox seeds an empty agent tab (via
// materializeRestorableTabs or its own late seeding) and must carry that handle
// onto the seeded tab so both the adoption launch and the checkpoint write keep
// it. Before the fix the seeded tab's Conversation was empty, so the adoption
// launch omitted the conversation id and the next checkpoint serialized a nil
// AgentConversation — permanently erasing the resume handle from
// instances.json.

const recordedConvID = "conv-recorded-123"

func recordedConversation() AgentConversationData {
	return AgentConversationData{Agent: "claude", ID: recordedConvID}
}

// A restorable shell row staged at reload — the exact shape of a sandbox session
// that has a surviving shell tab when its agent-server crashes.
func stagedRestorableTab() TabData {
	return TabData{ID: "survivor-1", Name: "shell", Kind: TabKindShell, SandboxTmux: "srv__shell"}
}

// --- Adoption launch (crashed-server restart) ---

// The main bug: staging restorable rows must not cause the adoption launch to
// omit the recorded conversation id. Before the fix, materializeRestorableTabs
// seeded an empty agent tab ahead of the launch, AgentConversation() returned
// that empty tab's Conversation, and the launch body carried no id — silently
// bypassing the no-guess guard and respawning the agent on the provider's
// latest conversation instead of the recorded one (#4797).
func TestReattachAdoptionLaunchCarriesTheRecordedConversationWithStagedTabs(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.ID = "reattach-staged"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.pendingMetadataTabs = []TabData{stagedRestorableTab()}
	inst.mu.Unlock()

	require.NoError(t, inst.ReattachSandbox())

	require.Len(t, h.lifecycle, 2, "provision + launch")
	assert.Contains(t, h.lifecycle[1], "launch")
	assert.Contains(t, h.lifecycle[1], recordedConvID,
		"the adoption launch must name the recorded conversation even when restorable rows are staged")
}

// The bare variant (no staged rows): the launch already carried the id before
// the fix (Tabs was empty through the launch, so AgentConversation fell back to
// reportedConversation). Pinned here as a regression guard.
func TestBareReattachAdoptionLaunchStillCarriesTheRecordedConversation(t *testing.T) {
	inst, p, h := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.ID = "reattach-bare"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.mu.Unlock()
	require.Empty(t, inst.Tabs, "the bare row holds its conversation on the handle, as a reloaded row does")

	require.NoError(t, inst.ReattachSandbox())
	require.Len(t, h.lifecycle, 2)
	assert.Contains(t, h.lifecycle[1], "launch")
	assert.Contains(t, h.lifecycle[1], recordedConvID)
}

// --- Persistence (memory projection + checkpoint) ---

// After a crashed-server reattach with staged tabs, the resume handle must
// survive in the row's memory projection. Before the fix, the seeded empty
// agent tab made toInstanceDataLocked write AgentConversation == nil (the
// else-if fallback was unreachable), so the next checkpoint erased the handle
// from instances.json.
func TestReattachKeepsTheResumeHandleInPersistenceWithStagedTabs(t *testing.T) {
	inst, p, _ := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.ID = "reattach-staged-persist"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.pendingMetadataTabs = []TabData{stagedRestorableTab()}
	inst.mu.Unlock()

	// Before the reattach, the reloaded row's projection carries the handle
	// via the reportedConversation fallback (Tabs is empty).
	preData := inst.ToInstanceData()
	require.NotNil(t, preData.AgentConversation, "a reloaded row with no tabs keeps the handle on reportedConversation")
	assert.Equal(t, recordedConvID, preData.AgentConversation.ID)

	require.NoError(t, inst.ReattachSandbox())

	// After the reattach, the handle must still be present — both on the agent
	// tab and in the row's projection.
	assert.True(t, inst.AgentConversation().HasID(),
		"AgentConversation() must report the recorded id after reattach")
	require.NotEmpty(t, inst.Tabs)
	assert.True(t, inst.Tabs[0].Conversation.HasID(),
		"the seeded agent tab must carry the recorded conversation")

	postData := inst.ToInstanceData()
	require.NotNil(t, postData.AgentConversation,
		"a checkpoint after reattach must not drop the resume handle")
	assert.Equal(t, recordedConvID, postData.AgentConversation.ID)

	// The handle must survive a full storage round trip.
	stored := inst.ToInstanceData()
	stored.ID = inst.ID
	stored.BackendType = "sandbox"
	stored.Liveness = LiveRunning
	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	assert.True(t, restored.AgentConversation().HasID(),
		"the resume handle survives the storage round trip")
}

// The bare variant: the late seeding in ReattachSandbox also installed an empty
// agent tab, so the persistence half of the drop affected even a row with no
// staged rows. A checkpoint landing between reattach and the first snapshot
// capture would erase the handle permanently.
func TestReattachKeepsTheResumeHandleInPersistenceBare(t *testing.T) {
	inst, p, _ := newCrashedHost(t, agentServerProvedDead)
	p.launchScript = "echo relaunched & echo $!"
	inst.ID = "reattach-bare-persist"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.mu.Unlock()

	require.NoError(t, inst.ReattachSandbox())

	assert.True(t, inst.AgentConversation().HasID(),
		"AgentConversation() must report the recorded id after a bare reattach")
	require.NotEmpty(t, inst.Tabs)
	assert.True(t, inst.Tabs[0].Conversation.HasID(),
		"the late-seeded agent tab must carry the recorded conversation")

	postData := inst.ToInstanceData()
	require.NotNil(t, postData.AgentConversation,
		"a checkpoint after a bare reattach must not drop the resume handle")
	assert.Equal(t, recordedConvID, postData.AgentConversation.ID)
}

// --- Non-crashed reattach (surviving server) ---

// A surviving-server reattach with staged tabs: materializeRestorableTabs seeds
// the agent tab, and the handle must travel onto it so the next checkpoint
// keeps it.
func TestNonCrashedReattachKeepsTheResumeHandleWithStagedTabs(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	inst.ID = "reattach-survive-staged"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.pendingMetadataTabs = []TabData{stagedRestorableTab()}
	inst.mu.Unlock()

	require.NoError(t, inst.ReattachSandbox())

	assert.True(t, inst.AgentConversation().HasID(),
		"AgentConversation() must report the recorded id after a surviving-server reattach with staged tabs")
	require.NotEmpty(t, inst.Tabs)
	assert.True(t, inst.Tabs[0].Conversation.HasID(),
		"the seeded agent tab must carry the recorded conversation")

	postData := inst.ToInstanceData()
	require.NotNil(t, postData.AgentConversation,
		"a checkpoint after a surviving-server reattach with staged tabs must not drop the handle")
	assert.Equal(t, recordedConvID, postData.AgentConversation.ID)
}

// A surviving-server reattach without staged rows: the late seeding installs the
// agent tab, and the handle must travel onto it.
func TestNonCrashedReattachKeepsTheResumeHandleBare(t *testing.T) {
	inst, _ := reattachFixture(t, "")
	inst.ID = "reattach-survive-bare"
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.mu.Unlock()

	require.NoError(t, inst.ReattachSandbox())

	assert.True(t, inst.AgentConversation().HasID(),
		"AgentConversation() must report the recorded id after a bare surviving-server reattach")
	require.NotEmpty(t, inst.Tabs)
	assert.True(t, inst.Tabs[0].Conversation.HasID(),
		"the late-seeded agent tab must carry the recorded conversation")

	postData := inst.ToInstanceData()
	require.NotNil(t, postData.AgentConversation,
		"a checkpoint after a bare surviving-server reattach must not drop the handle")
	assert.Equal(t, recordedConvID, postData.AgentConversation.ID)
}

// --- Direct unit tests for the three fix sites ---

// materializeRestorableTabs must carry the reported conversation onto the
// seeded agent tab, mirroring remoteAgentBackend.Launch.
func TestMaterializeRestorableTabsCarriesTheReportedConversation(t *testing.T) {
	inst := &Instance{Title: "unit-staged"}
	inst.mu.Lock()
	inst.reportedConversation = recordedConversation()
	inst.pendingMetadataTabs = []TabData{stagedRestorableTab()}
	inst.mu.Unlock()

	inst.materializeRestorableTabs()

	require.NotEmpty(t, inst.Tabs, "an agent tab is seeded")
	assert.Equal(t, TabKindAgent, inst.Tabs[0].Kind)
	assert.True(t, inst.Tabs[0].Conversation.HasID(),
		"the seeded agent tab carries the recorded conversation")
	assert.Equal(t, recordedConvID, inst.Tabs[0].Conversation.ID)
	assert.True(t, inst.AgentConversation().HasID(),
		"AgentConversation() reads the handle from the seeded tab")
	assert.Empty(t, inst.pendingMetadataTabs, "the restorable row was drained")
	require.Len(t, inst.Tabs, 2, "the agent tab plus the restorable shell row")
	assert.Equal(t, TabKindShell, inst.Tabs[1].Kind)
}

// materializeRestorableTabs with no reported conversation seeds an empty tab
// (no handle to carry), and does not panic.
func TestMaterializeRestorableTabsSeedsEmptyWhenNoReportedConversation(t *testing.T) {
	inst := &Instance{Title: "unit-no-conv"}
	inst.pendingMetadataTabs = []TabData{stagedRestorableTab()}

	inst.materializeRestorableTabs()

	require.NotEmpty(t, inst.Tabs)
	assert.False(t, inst.Tabs[0].Conversation.HasID(),
		"no reported conversation means the seeded tab has no conversation")
}

// toInstanceDataLocked must fall back to reportedConversation when Tabs[0]
// has an empty conversation, so a reattach can never launder the handle out
// of instances.json.
func TestToInstanceDataFallsBackToReportedConversationForEmptyTab(t *testing.T) {
	inst := &Instance{Title: "unit-fallback"}
	inst.mu.Lock()
	inst.Tabs = []*Tab{newRemoteAgentTab()} // empty Conversation
	inst.reportedConversation = recordedConversation()
	inst.mu.Unlock()

	data := inst.ToInstanceData()
	require.NotNil(t, data.AgentConversation,
		"the fallback must keep the handle when the agent tab's conversation is empty")
	assert.Equal(t, recordedConvID, data.AgentConversation.ID)
}

// toInstanceDataLocked must prefer Tabs[0].Conversation when it has an id, so
// a server-reported conversation that supersedes the recorded one is not
// overwritten by the stale reportedConversation.
func TestToInstanceDataPrefersTabConversationWhenPresent(t *testing.T) {
	fresh := AgentConversationData{Agent: "claude", ID: "conv-fresh-999"}
	inst := &Instance{Title: "unit-prefer"}
	inst.mu.Lock()
	inst.Tabs = []*Tab{{Kind: TabKindAgent, Conversation: fresh}}
	inst.reportedConversation = recordedConversation()
	inst.mu.Unlock()

	data := inst.ToInstanceData()
	require.NotNil(t, data.AgentConversation)
	assert.Equal(t, "conv-fresh-999", data.AgentConversation.ID,
		"the tab's own conversation wins over the stale reportedConversation")
}

// A row with no tabs and no reported conversation serializes no conversation —
// the fallback does not invent one.
func TestToInstanceDataOmitsConversationWhenNoneExists(t *testing.T) {
	inst := &Instance{Title: "unit-none"}
	inst.Tabs = []*Tab{newRemoteAgentTab()}

	data := inst.ToInstanceData()
	assert.Nil(t, data.AgentConversation)
}

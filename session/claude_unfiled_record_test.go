package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7215: a recorded Claude id whose store lists cleanly but holds no transcript
// for it is one Claude never filed (it files a conversation lazily, on its
// first entry) or one its retention deleted. `claude --resume` of such an id
// exits at launch with "No conversation found with session ID", so every
// launch of a recorded Claude id starts Claude ON it instead: the record stays
// truthful, and the first prompt files it. The case is reached by a handoff
// whose incoming record was made durable before its spawn, and the daemon
// stopped before the incoming agent's first prompt.

// recordedClaudeRespawn loads data through a tmux double whose sessions are
// all gone and returns the restored instance, the recording of every spawn,
// and the agent pane's name.
func recordedClaudeRespawn(t *testing.T, status Status, id string) (InstanceData, *[]string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	const agentName = "af_claude_unfiled"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	spawns := &[]string{}
	exec := recordingExec(map[string]bool{}, &newSessions, spawns)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, status, agentName, shellName)
	data.Tabs[0].Conversation = &AgentConversationData{
		Agent: tmux.ProgramClaude, ID: id, CaptureKind: ConversationCaptureInjected,
	}
	// The store lists cleanly: the worktree's project directory holds another,
	// older conversation, and none for the recorded id.
	project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), claudeRecordLine(t, data.Worktree.WorktreePath)), time.Now().Add(-time.Hour))
	return data, spawns, agentName
}

func TestRecoverStartsClaudeOnARecordedIDNoTranscriptHolds(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	data, spawns, agentName := recordedClaudeRespawn(t, Lost, carryTestID)

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	require.NoError(t, restored.Recover())

	spawn := agentSpawnOf(*spawns, agentName)
	require.NotEmpty(t, spawn, "Recover must re-spawn the agent")
	require.NotContains(t, spawn, "--resume", "--resume of an unfiled id exits at launch")
	require.Contains(t, spawn, "--session-id "+carryTestID, "the launch starts Claude on the recorded id")
	require.Equal(t, carryTestID, restored.AgentConversation().ID, "the recorded id stays the record")
}

// A handoff made its incoming record durable before the spawn, and the daemon
// stopped between that write and the spawn: the row names the incoming agent,
// its conversation N (never filed) and the mission it is owed. The load
// starts the incoming agent on N, not on a --resume that exits.
func TestLoadStartsAnInterruptedHandoffsConversationOnItsID(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	data, spawns, agentName := recordedClaudeRespawn(t, Ready, carryTestID)
	data.PendingHandoffMission = "You are continuing work that is already in progress in this worktree."
	data.HandoffDeliveryStatus = PromptNotDelivered

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	spawn := agentSpawnOf(*spawns, agentName)
	require.NotEmpty(t, spawn, "the load must re-spawn the incoming agent")
	require.NotContains(t, spawn, "--resume")
	require.Contains(t, spawn, "--session-id "+carryTestID)
	require.Equal(t, tmux.ProgramClaude, restored.AgentProgram(), "the row names the incoming agent")
	require.Equal(t, carryTestID, restored.AgentConversation().ID)
	require.NotEmpty(t, restored.PendingHandoffMission(), "the mission is still owed")
}

// #7209 review R2-2: a first launch's pin that is the tab's own record is
// judged by the same rule; a pin judged where it was set is not judged again.
func TestFirstLaunchJudgesARecordPinByTheRecordedClaudeRule(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	const freshID = "0e0e0e0e-1111-4222-8333-444444444444"
	for _, tc := range []struct {
		name   string
		record bool
		layout func(file func(id string, at time.Time))
		want   string
	}{
		{"an unfiled record pin starts on its id", true, func(func(string, time.Time)) {},
			"claude --session-id " + carryTestID},
		{"a current record pin resumes", true, func(file func(string, time.Time)) {
			file(carryTestID, past)
		}, "claude --resume " + carryTestID},
		{"a record pin a newer conversation superseded starts fresh", true, func(file func(string, time.Time)) {
			file(carryTestID, past)
			file(newerConversationID, past.Add(time.Minute))
		}, "claude --session-id " + freshID},
		{"a pin judged where it was set resumes", false, func(file func(string, time.Time)) {
			file(carryTestID, past)
			file(newerConversationID, past.Add(time.Minute))
		}, "claude --resume " + carryTestID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			workDir, recorded, _ := claudeRestoreWorktree(t)
			tc.layout(func(id string, at time.Time) {
				setModTime(t, writeCarryFile(t, configDir, filepath.Join(recorded, id+".jsonl"), claudeRecordLine(t, workDir)), at)
			})
			inst := claudeRestoreInstance(t, workDir, carryTestID)
			inst.Tabs = []*Tab{{}}
			pin := AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected}
			if tc.record {
				inst.SetResumeRecordedConversation(pin)
			} else {
				// The root heal's pin (InstanceOptions.ResumeConversation), judged
				// where it was set.
				inst.carriedConversation, inst.carriedIsRecord = pin, false
			}

			got, conv := planCreateConversation(inst, "claude", freshID)
			require.Equal(t, tc.want, got)
			require.Equal(t, strings.Fields(tc.want)[2], conv.ID, "the launch records the conversation it starts on")
		})
	}
}

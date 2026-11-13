package session

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7209 review R2-1: a load that refuses a stale Claude id and re-spawns a fresh
// one stages the replacement as the record before its pane can write (#7209
// review R6). It must also be durable by then. The daemon's loader used to
// persist it only after every row of every repo had loaded and spawned, so a
// daemon that stopped mid-load brought the row back on the refused id beside a
// pane running the replacement. The write runs on the re-spawn branch only: a
// load that reattaches a live pane must not record a replacement it never ran.

// staleClaudeLoad is a row whose recorded Claude id X is stale in an af-owned
// worktree, loaded through a tmux double; alive keeps its panes running.
func staleClaudeLoad(t *testing.T, alive bool) (InstanceData, *[]string, string) {
	t.Helper()
	return staleClaudeLoadAs(t, alive, Ready)
}

// staleClaudeLoadAs is staleClaudeLoad for a row persisted at status.
func staleClaudeLoadAs(t *testing.T, alive bool, status Status) (InstanceData, *[]string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	const agentName = "af_load_claude_durable"
	shellName := agentName + tmuxTabSeparator + shellTabName
	live := map[string]bool{}
	if alive {
		live = map[string]bool{agentName: true, shellName: true}
	}
	var newSessions int
	spawns := &[]string{}
	exec := recordingExec(live, &newSessions, spawns)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, status, agentName, shellName)
	data.Tabs[0].Conversation = &AgentConversationData{
		Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected,
	}
	project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))
	return data, spawns, agentName
}

func TestLoadRespawnRecordsItsReplacementDurablyBeforeTheSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	data, spawns, agentName := staleClaudeLoad(t, false)

	var writes []AgentConversationData
	rc := NewLoadRestoreContext()
	rc.SetPersistBeforeRespawn(func(i *Instance) error {
		require.Empty(t, agentSpawnOf(*spawns, agentName), "the write precedes the agent's spawn")
		writes = append(writes, i.AgentConversation())
		return nil
	})
	restored, err := FromInstanceDataWithContext(data, rc)
	require.NoError(t, err)

	spawn := agentSpawnOf(*spawns, agentName)
	require.NotEmpty(t, spawn, "the load re-spawns the missing agent session")
	require.Len(t, writes, 1, "the replacement is written once, before the spawn")
	require.NotEqual(t, carryTestID, writes[0].ID, "the durable record names the replacement, not the refused id")
	require.Contains(t, spawn, "--session-id "+writes[0].ID, "the spawn runs the conversation the durable record names")
	require.Equal(t, writes[0].ID, restored.AgentConversation().ID)
}

func TestLoadReattachWritesNoReplacement(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	data, spawns, agentName := staleClaudeLoad(t, true)

	called := false
	rc := NewLoadRestoreContext()
	rc.SetPersistBeforeRespawn(func(*Instance) error {
		called = true
		return nil
	})
	restored, err := FromInstanceDataWithContext(data, rc)
	require.NoError(t, err)
	require.Empty(t, agentSpawnOf(*spawns, agentName), "a live pane is reattached")
	require.False(t, called, "a reattached pane never ran a replacement, so none is written")
	require.Equal(t, carryTestID, restored.AgentConversation().ID)
}

func TestLoadRespawnThatCannotRecordItsReplacementSpawnsNothing(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	data, spawns, agentName := staleClaudeLoad(t, false)

	rc := NewLoadRestoreContext()
	rc.SetPersistBeforeRespawn(func(*Instance) error { return errors.New("disk is full") })
	restored, err := FromInstanceDataWithContext(data, rc)
	require.NoError(t, err, "the row loads, deferred to Recover")
	require.Empty(t, agentSpawnOf(*spawns, agentName), "nothing starts that the record does not name")
	require.Equal(t, Lost, restored.GetStatus(), "Recover retries it with its own persist hook")
	require.Equal(t, carryTestID, restored.AgentConversation().ID, "the record is put back")
}

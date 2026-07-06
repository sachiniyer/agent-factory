package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func writeDaemonCodexRolloutFile(t *testing.T, codexHome, name string) {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0644))
}

func TestCaptureAgentConversationPersistsCodexRolloutID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "codex-worker",
		Path:    repoPath,
		Program: tmux.ProgramCodex,
	})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSession("codex-worker", tmux.ProgramCodex))
	inst.SetStatus(session.Running)
	key := daemonInstanceKey(repo.ID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(repo.ID, inst.ToInstanceData()))

	snap := session.BeginConversationCapture()
	writeDaemonCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	manager.captureAgentConversation(repo.ID, key, inst, snap, time.Second)

	raw, err := config.LoadRepoInstances(repo.ID)
	require.NoError(t, err)
	var stored []session.InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Len(t, stored, 1)
	require.NotNil(t, stored[0].AgentConversation)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", stored[0].AgentConversation.ID)
	require.Len(t, stored[0].Tabs, 1)
	require.NotNil(t, stored[0].Tabs[0].Conversation)
	require.Equal(t, stored[0].AgentConversation.ID, stored[0].Tabs[0].Conversation.ID)
}

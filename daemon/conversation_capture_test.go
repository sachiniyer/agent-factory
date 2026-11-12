package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureAgentConversation runs one capture attempt the way the loop's first
// attempt does — the loop's snapshot, no baseline retake — and commits any id
// it finds. It is a test helper: the loop calls captureAgentConversationAttempt
// directly (#7140).
func (m *Manager) captureAgentConversation(repoID, key string, inst *session.Instance, snap session.ConversationCaptureSnapshot, token session.AgentRuntimeToken, timeout time.Duration) error {
	return m.captureAgentConversationAttempt(repoID, key, inst, &snap, token, timeout, false)
}

// Rollouts these fixtures write must sit in the day dir a real recorder
// would mint them in now — the bounded capture scan prunes directories
// provably older than the snapshot window, so a file dated in the past is
// invisible to the very capture path the fixture feeds (#6845).
func writeDaemonCodexRolloutFile(t *testing.T, codexHome, name string) {
	t.Helper()
	path := filepath.Join(configAgentCodexDayDir(codexHome, time.Now()), name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0644))
}

func writeDaemonCodexRolloutFileWithCwd(t *testing.T, codexHome, name, cwd string) {
	t.Helper()
	path := filepath.Join(configAgentCodexDayDir(codexHome, time.Now()), name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": cwd},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0644))
}

// heldAllCodexRollouts answers capture attribution's question — "does the
// runtime hold this rollout open" — with a live view of the fixture store:
// every rollout the test wrote is the session's own, exactly as a real pane's
// Codex would hold every file it minted. It re-globs on each judgement, so a
// rollout written after the arm is held the moment it exists (#6847).
func heldAllCodexRollouts(codexHome string) func() (map[string]struct{}, error) {
	return func() (map[string]struct{}, error) {
		paths, err := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*.jsonl"))
		if err != nil {
			return nil, err
		}
		held := make(map[string]struct{}, len(paths))
		for _, path := range paths {
			held[path] = struct{}{}
		}
		return held, nil
	}
}

func TestCaptureAgentConversationPersistsCodexRolloutID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
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
	// Attribution needs a runtime holding the rollout open; the fixture's
	// pane is a name-only handle, so the test hands the walk its store.
	inst.SetCodexRuntimeHeldOverrideForTest(heldAllCodexRollouts(codexHome))
	inst.SetStatusForTest(session.Running)
	key := daemonInstanceKey(repo.ID, inst.Title)
	manager.mu.Lock()
	manager.instances[key] = inst
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(repo.ID, inst.ToInstanceData()))

	snap := session.BeginConversationCapture()
	writeDaemonCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	manager.captureAgentConversation(repo.ID, key, inst, snap, inst.AgentRuntimeToken(), time.Second)

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

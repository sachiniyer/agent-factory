package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The #6769 operator-repair suite. set-conversation writes an Agent-tab
// conversation id only after the session's own rollout store proves the id
// belongs to it — every branch below is one of the strict refusals or the one
// admitted write.

// setConversationFixture is a tracked live codex row with a real worktree, so
// the validator's launch-directory correlation resolves exactly the way the
// create-time capture would.
func setConversationFixture(t *testing.T) (*Manager, *session.Instance, string, string) {
	t.Helper()
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return manager, inst, repoID, codexHome
}

const setConversationDaemonID = "019f386f-7206-7fc2-803b-f7045e07a299"

// writeDaemonCodexRolloutFileWithSource is the WithCwd writer plus a
// session_meta source — the sub-agent marker the validator must refuse.
func writeDaemonCodexRolloutFileWithSource(t *testing.T, codexHome, name, cwd string, source any) {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", "2026", "08", "10", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": cwd, "source": source},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0644))
}

func setConversationReq(inst *session.Instance, repoID, id string) SetConversationRequest {
	return SetConversationRequest{Title: inst.Title, RepoID: repoID, ConversationID: id}
}

func TestSetConversationRecordsAProvenRollout(t *testing.T) {
	manager, inst, repoID, codexHome := setConversationFixture(t)
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl", inst.Path)

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil))
	conv := inst.AgentConversation()
	assert.Equal(t, setConversationDaemonID, conv.ID)
	assert.Equal(t, "codex_rollout", conv.CaptureKind)
	assert.Equal(t, setConversationDaemonID, persistedConversationID(t, repoID))

	// Same id again is a no-op success, not a rewrite.
	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil))
}

func TestSetConversationRefusesUnprovenIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		setup  func(t *testing.T, codexHome, workDir string)
		needle string
	}{
		{"empty", "", nil, "a conversation id is required"},
		{"malformed", "not-a-uuid", nil, "is not a Codex conversation id"},
		{"absent", setConversationDaemonID, nil, "no rollout named"},
		{
			"unclassifiable", setConversationDaemonID,
			func(t *testing.T, home, _ string) {
				writeDaemonCodexRolloutFile(t, home, "rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl")
			},
			"cannot be read or classified",
		},
		{
			"foreign worktree", setConversationDaemonID,
			func(t *testing.T, home, _ string) {
				writeDaemonCodexRolloutFileWithCwd(t, home, "rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl", t.TempDir())
			},
			"belongs to a different session",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, inst, repoID, codexHome := setConversationFixture(t)
			if tc.setup != nil {
				tc.setup(t, codexHome, inst.Path)
			}
			err := manager.setConversation(setConversationReq(inst, repoID, tc.id), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.needle)
			assert.Empty(t, inst.AgentConversation().ID, "a refused id must leave the record untouched")
		})
	}
}

func TestSetConversationRefusesSubAgentRollout(t *testing.T) {
	manager, inst, repoID, codexHome := setConversationFixture(t)
	writeDaemonCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl", inst.Path,
		map[string]any{"subagent": "review"})

	err := manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sub-agent thread")
}

func TestSetConversationRefusesNonCodex(t *testing.T) {
	manager, inst, repoID, _ := setConversationFixture(t)
	claude, err := session.NewInstance(session.InstanceOptions{
		Title: "claude-worker", Path: inst.Path, Program: "claude",
	})
	require.NoError(t, err)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repoID, claude.Title)] = claude
	manager.mu.Unlock()
	require.NoError(t, appendInstanceData(repoID, claude.ToInstanceData()))

	err = manager.setConversation(setConversationReq(claude, repoID, setConversationDaemonID), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can only inspect a Codex")
}

func TestSetConversationRefusesDuringAnOperation(t *testing.T) {
	manager, inst, repoID, codexHome := setConversationFixture(t)
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl", inst.Path)
	inst.SetInFlightOpForTest(session.OpRespawning)

	err := manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "respawn")
	assert.Empty(t, inst.AgentConversation().ID, "a mid-op refusal must not write")

	// Once the operation settles the same id is admitted.
	inst.SetInFlightOpForTest(session.OpNone)
	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil))
}

// An id recorded while a capture attempt polled the store — an operator's
// repair — stands: the daemon's commit re-checks under the op-lock, and the
// rollout the poll discovered must not replace the record (#6769 review).
// The baseline is taken empty, then two rollouts land: the repair's (which
// set-conversation validates against) and a second one the capture poll
// discovers as new.
func TestCaptureAgentConversationSkipsARecordedRepair(t *testing.T) {
	shrinkConversationCaptureForTest(t)
	manager, inst, repoID, key, codexHome := codexLoopTestInstance(t)
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-"+setConversationDaemonID+".jsonl", inst.Path)

	snap := session.BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, inst.Path)
	writeDaemonCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-18-00-029f386f-7206-7fc2-803b-f7045e07a299.jsonl", inst.Path)

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, setConversationDaemonID), nil))
	require.Equal(t, setConversationDaemonID, inst.AgentConversation().ID)

	require.NoError(t, manager.captureAgentConversation(repoID, key, inst, snap,
		inst.AgentRuntimeToken(), 50*time.Millisecond))
	require.Equal(t, setConversationDaemonID, inst.AgentConversation().ID,
		"a capture that discovered a rollout must not replace the operator's record")
}

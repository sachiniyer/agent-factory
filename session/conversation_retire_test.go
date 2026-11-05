package session

// The #6769 review follow-ups: a refused resume that commits a fresh pane
// must retire the dead recorded id and hand the deferred arm a pre-spawn
// baseline, and the deferred baseline must bind the running command's
// evidence — not a re-resolution a config edit can move — on a local
// workspace only.

import (
	"testing"

	"github.com/stretchr/testify/require"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func retireTestInstance(t *testing.T) *Instance {
	t.Helper()
	repoPath := initTempGitRepo(t)
	inst, err := NewInstance(InstanceOptions{Title: "codex-worker", Path: repoPath, Program: tmux.ProgramCodex})
	require.NoError(t, err)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, repoPath, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.Tabs = []*Tab{{Conversation: AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242",
	}}}
	return inst
}

func TestRetireConversationForFreshRuntimeDropsTheRefusedID(t *testing.T) {
	inst := retireTestInstance(t)
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", ConversationCaptureSnapshot{})
	require.False(t, inst.AgentConversation().HasID(),
		"the refused id must be retired so the next capture arm is not suppressed")
}

func TestRetireConversationForFreshRuntimeKeepsNewerEvidence(t *testing.T) {
	inst := retireTestInstance(t)
	inst.SetAgentConversation(AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5299e00d-1111-4222-8333-f7045e07a242",
	})
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", ConversationCaptureSnapshot{})
	require.Equal(t, "5299e00d-1111-4222-8333-f7045e07a242", inst.AgentConversation().ID,
		"a conversation recorded between refusal and commit is newer evidence — never cleared")
}

func TestRetireConversationForFreshRuntimeParksThePreSpawnBaseline(t *testing.T) {
	inst := retireTestInstance(t)

	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", ConversationCaptureSnapshot{})
	_, ok := inst.TakePendingConversationCapture()
	require.False(t, ok, "a snapshot that cannot see a Codex store is never parked")

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	require.True(t, snap.CapturesCodex())
	inst.RetireConversationForFreshRuntime("5299e00d-1111-4222-8333-f7045e07a242", snap)

	parked, ok := inst.TakePendingConversationCapture()
	require.True(t, ok, "the pre-spawn baseline must be parked for the next deferred arm")
	require.True(t, parked.CapturesCodex())
	_, ok = inst.TakePendingConversationCapture()
	require.False(t, ok, "the parked baseline is consumed once")
}

func TestRespawnLaunchProgramReportsTheRefusedID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // empty store: the recorded rollout is missing
	inst := retireTestInstance(t)

	program, _, refusedID := respawnLaunchProgram(inst, "codex", "codex", false, true, nil)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", refusedID,
		"a stale recorded rollout must surface as the id the commit retires")
	require.NotContains(t, program, "resume",
		"the refused launch comes back fresh, not resume-rewritten")

	// The resume-accepted control: a real rollout for the recorded id resumes
	// exactly and refuses nothing.
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-10T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", inst.Path)
	program, _, refusedID = respawnLaunchProgram(inst, "codex", "codex", false, true, nil)
	require.Empty(t, refusedID)
	require.Contains(t, program, "resume 019f386f-7206-7fc2-803b-f7045e07a242")
}

func TestSessionCodexCaptureBaselineBindsTheRuntimeCommand(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	runtimeHome := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	inst := retireTestInstance(t)
	// The launch that produced this runtime carried an explicit store; a
	// config edit — or the ambient CODEX_HOME — since must not re-point the
	// baseline at a different one.
	inst.setRuntimeProgram("CODEX_HOME=" + runtimeHome + " codex")

	snap := inst.SessionCodexCaptureBaseline()
	require.True(t, snap.CapturesCodex(), "a runtime command with a resolvable store must baseline")
	require.Equal(t, runtimeHome, snap.codexHome,
		"the baseline must watch the store the running command declares, not the current environment's")
}

func TestSessionCodexCaptureBaselineRefusesARemoteWorkspace(t *testing.T) {
	inst := retireTestInstance(t)
	inst.SetBackend(remoteWorkspaceBackend{FakeBackend: &FakeBackend{}})
	snap := inst.SessionCodexCaptureBaseline()
	require.False(t, snap.CapturesCodex(),
		"a remote pane's provider store lives off-box; the daemon's registry is a different store")
}

func TestSessionCodexCaptureBaselineFallsBackWithoutRuntimeEvidence(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	inst := retireTestInstance(t)
	require.Empty(t, inst.RuntimeProgram())

	snap := inst.SessionCodexCaptureBaseline()
	require.True(t, snap.CapturesCodex(),
		"a pre-evidence row falls back to resolving the configured program")
	// The ambient home the fallback resolution names must be the scanned one.
	files, err := codexRolloutFiles(codexHome)
	require.NoError(t, err)
	require.Empty(t, files)
}

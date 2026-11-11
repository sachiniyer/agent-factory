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
	require.False(t, inst.PendingConversationCaptureParkedForTest(),
		"a snapshot that cannot see a Codex store is never parked")

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	require.True(t, snap.CapturesCodex())
	inst.RetireConversationForFreshRuntime("5299e00d-1111-4222-8333-f7045e07a242", snap)
	require.True(t, inst.PendingConversationCaptureParkedForTest(),
		"the pre-spawn baseline must be parked for the next deferred arm")
}

// TestPendingConversationCaptureReleasesOnlyToALaterGeneration is the #6946
// ordering guarantee: the respawn parks the baseline BEFORE bumping the
// runtime generation, so the arm for the generation the baseline predates
// must not spend it — its commits would die on the generation gate and take
// the only before-image that names the replacement's rollout with them.
func TestPendingConversationCaptureReleasesOnlyToALaterGeneration(t *testing.T) {
	inst := retireTestInstance(t)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", snap)

	park, current := inst.TakePendingConversationCaptureForRuntime(inst.AgentRuntimeToken())
	require.True(t, current)
	require.Nil(t, park, "the arm for the generation the baseline predates must not spend it")
	require.True(t, inst.PendingConversationCaptureParkedForTest(),
		"the decline leaves the park in place")

	inst.noteAgentRuntimeReplaced()

	park, current = inst.TakePendingConversationCaptureForRuntime(inst.AgentRuntimeToken())
	require.True(t, current)
	require.NotNil(t, park, "the replacement generation's arm spends the park")
	require.True(t, park.Snapshot.CapturesCodex())
	require.False(t, inst.PendingConversationCaptureParkedForTest(),
		"the release consumes the park")
}

// TestPendingConversationCaptureRefusesAStaleToken: a token whose generation
// the runtime has already left releases nothing and consumes nothing — the
// park survives the stale arm's decline for the arm the replacement sends.
func TestPendingConversationCaptureRefusesAStaleToken(t *testing.T) {
	inst := retireTestInstance(t)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	stale := inst.AgentRuntimeToken()
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", snap)

	inst.noteAgentRuntimeReplaced()

	park, current := inst.TakePendingConversationCaptureForRuntime(stale)
	require.False(t, current, "a token from a dead generation declines")
	require.Nil(t, park)
	require.True(t, inst.PendingConversationCaptureParkedForTest(),
		"a stale arm's decline cannot consume the park")

	park, current = inst.TakePendingConversationCaptureForRuntime(inst.AgentRuntimeToken())
	require.True(t, current)
	require.NotNil(t, park, "the park survived for the live generation's arm")
}

// TestPendingConversationCaptureRestoreAndDrop: a consumed park goes back when
// its arm is refused mid-launch, and the permanently-moot paths retire it
// outright — the park's only consumer is an arm that actually proceeds.
func TestPendingConversationCaptureRestoreAndDrop(t *testing.T) {
	inst := retireTestInstance(t)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", snap)
	inst.noteAgentRuntimeReplaced()

	park, _ := inst.TakePendingConversationCaptureForRuntime(inst.AgentRuntimeToken())
	require.NotNil(t, park)
	inst.RestorePendingConversationCapture(park)
	require.True(t, inst.PendingConversationCaptureParkedForTest(),
		"a refused launch hands the baseline back to the next tick")

	inst.DropPendingConversationCapture()
	require.False(t, inst.PendingConversationCaptureParkedForTest(),
		"a permanently-moot row drops the park explicitly")
}

// TestDropPendingConversationCaptureIfMoot: the moot clear names its reasons —
// archived here — and a live unrecorded row keeps its park. The condition and
// the drop share one lock hold so nothing can park between them (#6946 review).
func TestDropPendingConversationCaptureIfMoot(t *testing.T) {
	inst := retireTestInstance(t)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(t.TempDir(), inst.Path)
	inst.RetireConversationForFreshRuntime("019f386f-7206-7fc2-803b-f7045e07a242", snap)

	require.False(t, inst.DropPendingConversationCaptureIfMoot(),
		"a live unrecorded row is not moot — the park stays")
	require.True(t, inst.PendingConversationCaptureParkedForTest())

	inst.SetStatusForTest(Archived)
	require.True(t, inst.DropPendingConversationCaptureIfMoot(),
		"an archived row can never arm again")
	require.False(t, inst.PendingConversationCaptureParkedForTest(),
		"the moot row's park is dropped")
}

func TestRespawnLaunchProgramReportsTheRefusedID(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // empty store: the recorded rollout is missing
	inst := retireTestInstance(t)

	program, _, refusedID, err := respawnLaunchProgram(inst, "codex", "codex", false, true, nil)
	require.NoError(t, err)
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
	program, _, refusedID, err = respawnLaunchProgram(inst, "codex", "codex", false, true, nil)
	require.NoError(t, err)
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

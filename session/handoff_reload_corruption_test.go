package session

import (
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReloadCorruptedRow_LoadsIncomingAgentFromDisk is the end-to-end reload
// half of the poll/handoff persist-race regression. The race's poll-written row
// is the exact shape this test loads: Program = <incoming agent> (gemini),
// PendingHandoffMission = "" (the mission marker is set only on the SUCCESS path,
// after SwapAgent returns), and InFlightOp = OpNone because ForStorage strips the
// transient op axis. An unclean exit before any later durable write would hand a
// fresh daemon exactly this record.
//
// This test exercises the PRODUCTION reload path via the restoreTmuxSession
// package var (instance_data.go:565 — "a package var so restore-survival tests
// can inject mock-backed sessions and stay hermetic; production uses the real
// constructor"), exactly as TestLiveInstance_ReattachesExistingSessionWithIdleEvidence
// (dead_respawn_test.go:507) does. The mock reports the agent tmux session as
// existing so LocalBackend.Start's restore takes the reattach branch — no real
// tmux exec, no respawn.
//
// It pins WHY the write prevention in the daemon-package regression (2.1) is
// load-bearing: a row of this shape reloads as the INCOMING agent with
// InFlightOp: OpNone and no delivery obligation, so the live tmux pane (still
// running the OUTGOING agent) would be masked by the wrong identity on every
// caller — AgentProgram(), ResolvedPaneProgram(), the TUI, the poll — and the
// takeover brief would be silently lost.
func TestReloadCorruptedRow_LoadsIncomingAgentFromDisk(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_corrupt_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// Mock tmux: the agent (and shell) sessions report existing, so
	// LocalBackend.Start's restore reattaches rather than respawning.
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// The EXACT shape the race produces on disk: incoming agent recorded as
	// Program, no mission marker, op axis stripped by ForStorage, Running liveness.
	const repoPath = "/tmp/handoff-corrupt-repo"
	data := InstanceData{
		Title:    "handoff-corrupt",
		Path:     repoPath,
		Program:  tmux.ProgramGemini, // incoming agent — the corruption
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		PendingHandoffMission: "", // no mission marker — the takeover brief is gone
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(t.TempDir(), "wt"), // non-empty: reattach path
			SessionName:  "handoff-corrupt",
			BranchName:   "corrupt-branch",
		},
	}

	restored, err := FromInstanceData(data)
	require.NoError(t, err, "a corrupted row of the race's shape must still load — it is valid, just wrong")
	assert.Zero(t, newSessions, "a live persisted agent must be reattached, not re-spawned")

	// The wrong-agent identity is read from disk (Program), not probed from the
	// live pane. This is the customer-visible wrong-agent state.
	assert.Equal(t, tmux.ProgramGemini, restored.AgentProgram(),
		"AgentProgram() must read data.Program from disk; the race wrote the incoming agent, "+
			"and the live pane (still the outgoing agent) is never consulted")
	assert.Equal(t, tmux.ProgramGemini, restored.ResolvedPaneProgram(),
		"ResolvedPaneProgram() must read the tmux handle's program, which restoreLocalTabs fed "+
			"from data.Program — not a `tmux display-message` probe of the live pane")
	// ForStorage stripped the op axis, so the loader sees OpNone. The empty
	// PendingHandoffMission means the OpReplacing reconstruction at
	// instance_data.go:297-299 does NOT fire — there is no fence to reconstruct.
	assert.Equal(t, OpNone, restored.GetInFlightOp(),
		"ForStorage stripped InFlightOp to OpNone and the empty mission means no OpReplacing "+
			"reconstruction: the row loads as a settled (not mid-handoff) state")
	assert.Equal(t, "", restored.PendingHandoffMission(),
		"the takeover brief is silently lost: no fence is reconstructed from a mission-free record")
}

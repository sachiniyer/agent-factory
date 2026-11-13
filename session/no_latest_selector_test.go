package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7217 Codex P1: a launch that starts Claude on a recorded or minted
// conversation id (`--session-id N`) must never also carry a latest-conversation
// selector. Restore's re-spawn rewrite appended `--continue` to any Claude
// program it did not see a selector in, and it did not count --session-id as
// one. `--continue` loads the most recent conversation in the directory, which
// after a /clear is the conversation the user left; and Claude refuses
// `--session-id` beside `--continue` outright. Every path that launches on an
// id is checked by the command tmux actually spawns.

// agentCommandOf is the agent's own command inside a tmux spawn: the program
// af's session-env wrapper execs, up to the next tmux command. Everything
// before it is tmux's (new-session's -c is its start directory).
func agentCommandOf(spawn string) string {
	i := strings.Index(spawn, "__af-session-env-exec")
	if i < 0 {
		return spawn
	}
	agent := spawn[i:]
	if j := strings.Index(agent, " ; "); j >= 0 {
		agent = agent[:j]
	}
	return agent
}

func requireStartsOnIDWithoutALatestSelector(t *testing.T, spawn string) {
	t.Helper()
	require.NotEmpty(t, spawn, "the path must spawn its agent")
	agent := agentCommandOf(spawn)
	require.Contains(t, agent, "--session-id ", "the path starts Claude on an id: %s", agent)
	require.NotContains(t, agent, "--continue", "a launch on an id must not also ask for the latest conversation: %s", agent)
	for _, tok := range strings.Fields(agent) {
		require.NotEqual(t, "-c", strings.Trim(tok, `'"`), "a launch on an id must not also ask for the latest conversation: %s", agent)
	}
}

func TestNoLaunchOnAnIDGetsALatestConversationSelector(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spawn func(t *testing.T) string
	}{
		{"recover on an unfiled recorded id (and the archived restore's re-spawn)", func(t *testing.T) string {
			data, spawns, agentName := recordedClaudeRespawn(t, Lost, carryTestID)
			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			require.NoError(t, restored.Recover())
			return agentSpawnOf(*spawns, agentName)
		}},
		{"recover replacing a refused stale id", func(t *testing.T) string {
			data, spawns, agentName := staleClaudeLoadAs(t, false, Lost)
			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			require.NoError(t, restored.Recover())
			return agentSpawnOf(*spawns, agentName)
		}},
		{"load of an interrupted handoff's unfiled id", func(t *testing.T) string {
			data, spawns, agentName := recordedClaudeRespawn(t, Ready, carryTestID)
			data.PendingHandoffMission = "continue the task"
			data.HandoffDeliveryStatus = PromptNotDelivered
			_, err := FromInstanceData(data)
			require.NoError(t, err)
			return agentSpawnOf(*spawns, agentName)
		}},
		{"load replacing a refused stale id", func(t *testing.T) string {
			data, spawns, agentName := staleClaudeLoad(t, false)
			_, err := FromInstanceDataWithContext(data, NewLoadRestoreContext())
			require.NoError(t, err)
			return agentSpawnOf(*spawns, agentName)
		}},
		{"committed swap re-minted", func(t *testing.T) string {
			inst, _, spawns := remintedCommittedSwap(t)
			require.NoError(t, inst.RespawnForAccountSwap())
			for _, spawn := range *spawns {
				if strings.Contains(spawn, "--session-id") {
					return spawn
				}
			}
			return ""
		}},
		{"first launch on a record pin", func(t *testing.T) string {
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			workDir, _, _ := claudeRestoreWorktree(t)
			inst := claudeRestoreInstance(t, workDir, carryTestID)
			inst.Tabs = []*Tab{{}}
			inst.SetResumeRecordedConversation(AgentConversationData{
				Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected,
			})
			program, _ := planCreateConversation(inst, "claude", "0e0e0e0e-1111-4222-8333-444444444444")
			var newSessions int
			var spawns []string
			exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
			ts := tmux.NewTmuxSessionWithDeps("first-launch", program, persistPtyFactory{t: t, cmdExec: exec}, exec)
			require.NoError(t, ts.Start(workDir))
			return agentSpawnOf(spawns, "first-launch")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			requireStartsOnIDWithoutALatestSelector(t, tc.spawn(t))
		})
	}
}

// The handoff path, on its own: its pty double names a file after the test,
// which a subtest's slash would break.
func TestHandoffToClaudeGetsNoLatestConversationSelector(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	f := newBeforeStartSwap(t)
	_, err := f.inst.SwapAgent(f.plan)
	require.NoError(t, err)
	require.NotEmpty(t, f.ptys.cmds)
	requireStartsOnIDWithoutALatestSelector(t, strings.Join(f.ptys.cmds[len(f.ptys.cmds)-1].Args, " "))
}

// Controls: a program that carries no conversation flag is unchanged. A restore
// of a row that recorded no conversation still gets Claude's worktree-scoped
// --continue on its re-spawn, and a first launch is never rewritten at all.
func TestALaunchWithNoConversationFlagIsUnchanged(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Run("restore with nothing recorded resumes Claude's latest in the worktree", func(t *testing.T) {
		data, spawns, agentName := recordedClaudeRespawn(t, Lost, carryTestID)
		data.Tabs[0].Conversation = nil
		restored, err := FromInstanceData(data)
		require.NoError(t, err)
		require.NoError(t, restored.Recover())
		agent := agentCommandOf(agentSpawnOf(*spawns, agentName))
		require.NotContains(t, agent, "--session-id")
		require.Contains(t, agent, "--continue")
	})
	t.Run("first launch of a bare program", func(t *testing.T) {
		var newSessions int
		var spawns []string
		exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
		ts := tmux.NewTmuxSessionWithDeps("bare-launch", "claude", persistPtyFactory{t: t, cmdExec: exec}, exec)
		require.NoError(t, ts.Start(t.TempDir()))
		agent := agentCommandOf(agentSpawnOf(spawns, "bare-launch"))
		require.Equal(t, "__af-session-env-exec claude 0 claude", agent, "a first launch spawns its program as given")
	})
}

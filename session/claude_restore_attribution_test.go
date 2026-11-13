package session

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmd_test "github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7209 review: who a newer conversation belongs to (R1), which records the
// guard judges (R2), what a refused rebuild says (R5), and when the
// replacement becomes the record (R6).

func TestNewerConversationIsEvidenceOnlyInADirectoryAfOwns(t *testing.T) {
	repo, worktree := t.TempDir(), t.TempDir()
	row := func(repoPath, worktreePath string, external bool) *Instance {
		t.Helper()
		gw, err := sessiongit.NewGitWorktreeFromStorage(repoPath, worktreePath, "t", "main", "", external, true)
		require.NoError(t, err)
		return &Instance{Title: "t", gitWorktree: gw}
	}
	require.True(t, row(repo, worktree, false).newerConversationIsEvidence(), "a worktree af created for this session")
	require.False(t, row(repo, worktree, true).newerConversationIsEvidence(), "an adopted external worktree")
	require.False(t, row(repo, repo, false).newerConversationIsEvidence(), "the repository's own checkout")
	inPlace := row(repo, worktree, false)
	inPlace.inPlace = true
	require.False(t, inPlace.newerConversationIsEvidence(), "an `af sessions create --here` session")
	root := row(repo, worktree, false)
	root.Title = RootSessionTitle
	require.False(t, root.newerConversationIsEvidence(), "the root agent")
	require.False(t, (&Instance{Title: "t"}).newerConversationIsEvidence(), "no worktree")
}

// inPlaceRowData is a row working in the user's own checkout, recording
// conversation id for agent, with a newer conversation the user started there
// themselves.
func inPlaceRowData(t *testing.T, status Status, agent, agentName, id string) (InstanceData, string) {
	t.Helper()
	data := deadInstanceData(t, status, agentName, agentName+tmuxTabSeparator+shellTabName)
	checkout := data.Worktree.WorktreePath
	data.Path, data.Worktree.RepoPath, data.Worktree.ExternalWorktree = checkout, checkout, true
	data.Program = agent
	data.Tabs[0].Conversation = &AgentConversationData{Agent: agent, ID: id, CaptureKind: ConversationCaptureInjected}
	past := time.Now().Add(-time.Hour)
	switch agent {
	case tmux.ProgramClaude:
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		project := claudeProjectCandidates(checkout)[0]
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, id+".jsonl"), "{}\n"), past)
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))
	case tmux.ProgramCodex:
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		setModTime(t, writeCarryFile(t, codexHome, codexRolloutRel(id), codexRollout(checkout)), past)
		setModTime(t, writeCarryFile(t, codexHome, codexRolloutRel(newerConversationID), codexRollout(checkout)), past.Add(time.Minute))
	}
	return data, agentName
}

func agentSpawnOf(spawns []string, agentName string) string {
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, agentName+tmuxTabSeparator) {
			return spawn
		}
	}
	return ""
}

func useRecordingTmux(t *testing.T, exec cmd_test.MockCmdExec) {
	t.Helper()
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })
}

// R1: a row in the user's own checkout keeps and resumes its own conversation
// after a reboot and on Recover, though the user's own claude wrote a newer
// one there, as master did and as the root's own heal rules.
func TestAnInPlaceClaudeRowResumesItsOwnConversation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status Status
	}{{"reboot load", Ready}, {"recover", Lost}} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			var newSessions int
			var spawns []string
			useRecordingTmux(t, recordingExec(map[string]bool{}, &newSessions, &spawns))
			data, agentName := inPlaceRowData(t, tc.status, tmux.ProgramClaude, "af_inplace_claude", carryTestID)

			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			if tc.status == Lost {
				restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
				restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
				require.NoError(t, restored.Recover())
			}
			spawn := agentSpawnOf(spawns, agentName)
			require.NotEmpty(t, spawn)
			require.Contains(t, spawn, "--resume "+carryTestID, "the user's own conversation in the checkout is not this row's")
			require.Equal(t, carryTestID, restored.AgentConversation().ID)
		})
	}
}

// R1, Codex: the load-time retirement this PR added never retires an in-place
// row's record. The refusal itself predates it and is #7210's.
func TestAnInPlaceCodexRowKeepsItsRecordOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	var newSessions int
	var spawns []string
	useRecordingTmux(t, recordingExec(map[string]bool{}, &newSessions, &spawns))
	data, agentName := inPlaceRowData(t, Ready, tmux.ProgramCodex, "af_inplace_codex", carryTestID)

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.NotEmpty(t, agentSpawnOf(spawns, agentName))
	require.Equal(t, carryTestID, restored.AgentConversation().ID, "a row in the user's checkout retires nothing on load")
}

// R2: a sandbox restore delivers the tab's record to the agent-server as a
// pin, and the guard judges it like the tab's own record: after a /clear the
// relaunch does not resume the cleared conversation, records its replacement,
// and reports the refusal rather than a carry.
func TestAdoptedRelaunchJudgesTheTabsRecord(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	var newSessions int
	var spawns []string
	useRecordingTmux(t, recordingExec(map[string]bool{}, &newSessions, &spawns))

	const agentName = "af_adopted_claude"
	data := deadInstanceData(t, Lost, agentName, agentName+tmuxTabSeparator+shellTabName)
	project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	tabRecord := AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected}
	var outcome HandoffConversation
	require.NoError(t, restored.launchAdoptedWith(&tabRecord, func() error {
		outcome = restored.ResumeConversationOutcome()
		return restored.Recover()
	}))
	require.False(t, outcome.Carried, "%+v", outcome)
	require.Equal(t, staleCarryReason, outcome.CarryFailure)
	spawn := agentSpawnOf(spawns, agentName)
	require.NotEmpty(t, spawn)
	require.NotContains(t, spawn, "--resume", "the cleared conversation must not be resumed")
	requireReplacementRecorded(t, restored, spawn)
	require.False(t, restored.resumableConversation().ID == carryTestID, "the refused pin is spent")
}

// R5: a gone branch whose fresh rebuild would have to resume a refused record
// says why, rather than that no id was recorded.
func TestARefusedRebuildNamesTheRefusal(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	repo := initTempGitRepo(t)
	workDir := t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, workDir, "t", "af/gone", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title: "t", Program: tmux.ProgramClaude, Path: repo, gitWorktree: gw,
		Tabs: []*Tab{{Conversation: AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected}}},
	}
	project := claudeProjectCandidates(workDir)[0]
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))

	err = missingWorktreeRebuildRefusal(inst, gw, true, "claude")
	require.ErrorContains(t, err, "which af refused: "+staleCarryReason)
	require.NotContains(t, err.Error(), "no recorded conversation id")

	_, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.False(t, ok)
	require.Contains(t, freshRebuildRefusal(decline), staleCarryReason)
	require.Equal(t, "fresh rebuild requires a recorded conversation id for the resolved agent", freshRebuildRefusal(resumeDecline{}))
}

// staleClaudeRecoverRow is a Lost Claude row whose recorded conversation a
// newer one in its own worktree superseded.
func staleClaudeRecoverRow(t *testing.T, exec cmd_test.MockCmdExec, agentName string) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	useRecordingTmux(t, exec)
	data := deadInstanceData(t, Lost, agentName, agentName+tmuxTabSeparator+shellTabName)
	data.Tabs[0].Conversation = &AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected}
	project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
	return restored
}

// R6: the replacement is the durable record before its pane can write; a
// persist that fails launches nothing, and a spawn that fails puts the
// refused id back rather than leave a replacement with no transcript.
func TestARefusedRecoverRecordsItsReplacementBeforeTheSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	const agentName = "af_recover_replacement_durable"

	t.Run("durable before the spawn", func(t *testing.T) {
		var newSessions int
		var spawns []string
		restored := staleClaudeRecoverRow(t, recordingExec(map[string]bool{}, &newSessions, &spawns), agentName)
		var persisted []AgentConversationData
		var spawnsAtPersist []int
		restored.SetRelocationSettleNotify(func(i *Instance) error {
			persisted = append(persisted, i.AgentConversation())
			spawnsAtPersist = append(spawnsAtPersist, len(spawns))
			return nil
		})
		require.NoError(t, restored.Recover())
		require.NotEmpty(t, persisted)
		require.Equal(t, 0, spawnsAtPersist[0], "the record is persisted before the agent spawns")
		require.NotEqual(t, carryTestID, persisted[0].ID)
		require.Equal(t, restored.AgentConversation(), persisted[0], "what was persisted is what the pane runs")
		requireReplacementRecorded(t, restored, agentSpawnOf(spawns, agentName))
	})
	t.Run("a persist that fails launches nothing", func(t *testing.T) {
		var newSessions int
		var spawns []string
		restored := staleClaudeRecoverRow(t, recordingExec(map[string]bool{}, &newSessions, &spawns), agentName)
		restored.SetRelocationSettleNotify(func(*Instance) error { return errors.New("disk full") })
		require.ErrorContains(t, restored.Recover(), "could not be recorded durably")
		require.Empty(t, agentSpawnOf(spawns, agentName))
		require.Equal(t, carryTestID, restored.AgentConversation().ID)
	})
	t.Run("a spawn that fails puts the refused id back", func(t *testing.T) {
		var newSessions int
		var spawns []string
		inner := recordingExec(map[string]bool{}, &newSessions, &spawns)
		failing := inner
		failing.RunFunc = func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "new-session") && strings.Contains(cmd.String(), agentName) &&
				!strings.Contains(cmd.String(), agentName+tmuxTabSeparator) {
				return errors.New("tmux refused")
			}
			return inner.RunFunc(cmd)
		}
		restored := staleClaudeRecoverRow(t, failing, agentName)
		var persisted []AgentConversationData
		restored.SetRelocationSettleNotify(func(i *Instance) error {
			persisted = append(persisted, i.AgentConversation())
			return nil
		})
		require.Error(t, restored.Recover())
		require.Equal(t, carryTestID, restored.AgentConversation().ID, "no recorded replacement without a pane that ran it")
		require.NotEmpty(t, persisted)
		require.Equal(t, carryTestID, persisted[len(persisted)-1].ID, "and the restored record is persisted")
	})
}

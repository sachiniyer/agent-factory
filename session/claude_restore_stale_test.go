package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7206: a restore resumes a tab-recorded Claude conversation only while it is
// still the newest conversation in the worktree, under any spelling of the
// worktree's path, by the account swap's own rule (judgeClaudeRecorded).

// claudeRestoreWorktree is a worktree reached through a symlinked parent, and
// its two project-directory spellings: as recorded first, then as resolved.
func claudeRestoreWorktree(t *testing.T) (string, string, string) {
	t.Helper()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "repo"), 0o755))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	workDir := filepath.Join(link, "repo")
	projects := claudeProjectCandidates(workDir)
	require.Len(t, projects, 2, "the worktree must have two project-directory spellings")
	return workDir, projects[0], projects[1]
}

// claudeRestoreInstance is a Claude session whose agent tab recorded
// conversation id, in workDir: a worktree af created for it, apart from the
// repository's own checkout.
func claudeRestoreInstance(t *testing.T, workDir, id string) *Instance {
	t.Helper()
	repo := t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{
		Title:       "t",
		Program:     tmux.ProgramClaude,
		Path:        repo,
		gitWorktree: gw,
		Tabs: []*Tab{{Conversation: AgentConversationData{
			Agent: tmux.ProgramClaude, ID: id, CapturedAt: time.Now(), CaptureKind: ConversationCaptureInjected,
		}}},
	}
}

func TestExactResumeRefusesAClaudeConversationThatIsNoLongerTheNewest(t *testing.T) {
	const otherID = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name   string
		layout func(t *testing.T, file func(project, id string, at time.Time), recorded, resolved string)
		// refusal is the decline's reason; "" means the restore resumes.
		refusal string
		why     string
	}{
		{
			name: "a newer conversation under the same spelling",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(recorded, carryTestID, past)
				file(recorded, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "after a /clear the recorded id names the conversation the user abandoned",
		},
		{
			name: "a newer conversation under the resolved spelling",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(recorded, carryTestID, past)
				file(resolved, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "a /clear filed under the other spelling is still a newer conversation in this worktree",
		},
		{
			name: "a newer conversation under the recorded spelling",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(resolved, carryTestID, past)
				file(recorded, newerConversationID, past.Add(time.Minute))
			},
			refusal: staleCarryReason,
			why:     "the spelling the recorded transcript is under is not the only one Claude writes",
		},
		{
			name: "no newer conversation under either spelling",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(recorded, carryTestID, past.Add(time.Minute))
				file(resolved, newerConversationID, past)
			},
			why: "an older conversation does not make the recorded one stale",
		},
		{
			name: "a newer copy of the recorded conversation under the other spelling",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(recorded, carryTestID, past)
				file(resolved, carryTestID, past.Add(time.Minute))
			},
			why: "a copy of the same conversation is not another conversation",
		},
		{
			name: "a conversation newer than one copy but older than the newest copy",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(recorded, carryTestID, past)
				file(recorded, otherID, past.Add(time.Minute))
				file(resolved, carryTestID, past.Add(2*time.Minute))
			},
			why: "a resume by id continues the newest copy, and nothing is newer than that",
		},
		{
			name: "a recorded transcript af does not find",
			layout: func(t *testing.T, file func(string, string, time.Time), recorded, resolved string) {
				file(resolved, newerConversationID, past)
			},
			why: "a missing transcript says nothing about whether the user moved on, so the restore resumes as it always has",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			workDir, recorded, resolved := claudeRestoreWorktree(t)
			tc.layout(t, func(project, id string, at time.Time) {
				t.Helper()
				setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, id+".jsonl"), "{}\n"), at)
			}, recorded, resolved)
			inst := claudeRestoreInstance(t, workDir, carryTestID)

			rewritten, ok, decline := prepareExactResumeConversation(inst, "claude")
			if tc.refusal == "" {
				require.True(t, ok, tc.why)
				require.Equal(t, "claude --resume "+carryTestID, rewritten, tc.why)
				require.Empty(t, decline.reason, tc.why)
				return
			}
			require.False(t, ok, tc.why)
			require.Equal(t, "claude", rewritten, "a refused resume launches fresh")
			require.Equal(t, tc.refusal, decline.reason, tc.why)
			refusal := inst.exactResumeRefusal(decline)
			require.Equal(t, carryTestID, refusal.ID)
			require.True(t, refusal.retire, "the stale id is retired once the fresh spawn commits")
		})
	}
}

// A spelling af found but could not list is no evidence about the recorded
// conversation: the restore resumes it, as it does when the store cannot be
// named at all, and nothing is retired (#7209 review R4).
func TestExactResumeResumesAClaudeConversationItCannotCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recorded, resolved := claudeRestoreWorktree(t)
	writeCarryFile(t, configDir, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	unlistable := filepath.Join(configDir, resolved)
	require.NoError(t, os.MkdirAll(unlistable, 0o755))
	require.NoError(t, os.Chmod(unlistable, 0))
	t.Cleanup(func() { _ = os.Chmod(unlistable, 0o755) })

	rewritten, ok, decline := prepareExactResumeConversation(claudeRestoreInstance(t, workDir, carryTestID), "claude")
	require.True(t, ok, "%+v", decline)
	require.Equal(t, "claude --resume "+carryTestID, rewritten)
}

// An account-scoped session writes to the account's registry dir, not the
// ambient one its command resolves, and the guard reads that store.
func TestExactResumeJudgesAnAccountSessionByTheAccountsStore(t *testing.T) {
	ambient := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	account := registerAccount(t, tmux.ProgramClaude, "work")
	workDir, recorded, _ := claudeRestoreWorktree(t)
	past := time.Now().Add(-time.Hour)
	// The ambient store alone would say the recorded conversation is current.
	setModTime(t, writeCarryFile(t, filepath.Join(ambient, ".claude"), filepath.Join(recorded, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, account, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, account, filepath.Join(recorded, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))

	inst := claudeRestoreInstance(t, workDir, carryTestID)
	inst.Account = "work"
	_, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.False(t, ok, "the /clear happened in the account's store")
	require.Equal(t, staleCarryReason, decline.reason)
}

// A pin whose provenance was checked where it was set (a swap's plan, the root
// heal: SetResumeConversation) is not judged again. A pin that is the tab's
// record delivered from elsewhere is (TestAdoptedRelaunchJudgesTheTabsRecord).
func TestExactResumeLeavesAPinJudgedElsewhereAlone(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recorded, _ := claudeRestoreWorktree(t)
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n"), past)
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(recorded, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))

	inst := claudeRestoreInstance(t, workDir, carryTestID)
	inst.Tabs = []*Tab{{}}
	inst.SetResumeConversation(AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected})
	rewritten, ok, _ := prepareExactResumeConversation(inst, "claude")
	require.True(t, ok)
	require.Equal(t, "claude --resume "+carryTestID, rewritten)
}

// #7209 review: the refusal holds through the re-spawn itself. The command a
// recovery launches carries neither the refused id nor Claude's
// worktree-scoped --continue, which after a /clear would pick whichever
// conversation Claude wrote last, and the retry answer reports the refusal
// instead of predicting a carry.
func TestRecoverStartsAStaleClaudeSessionFresh(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{{"stale", true}, {"current", false}} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)

			const agentName = "af_recover_claude_stale"
			shellName := agentName + tmuxTabSeparator + shellTabName
			var newSessions int
			var spawns []string
			exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
			pty := persistPtyFactory{t: t, cmdExec: exec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
			}
			t.Cleanup(func() { restoreTmuxSession = prev })

			data := deadInstanceData(t, Lost, agentName, shellName)
			data.Tabs[0].Conversation = &AgentConversationData{
				Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected,
			}
			project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
			past := time.Now().Add(-time.Hour)
			other := past.Add(-time.Minute)
			if tc.stale {
				other = past.Add(time.Minute)
			}
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), other)

			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			require.Equal(t, Lost, restored.GetStatus())
			outcome := restored.ResumeConversationOutcome()
			require.NoError(t, restored.Recover())

			var agentSpawn string
			for _, spawn := range spawns {
				if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellName) {
					agentSpawn = spawn
					break
				}
			}
			require.NotEmpty(t, agentSpawn, "Recover must re-spawn the agent")
			if !tc.stale {
				require.True(t, outcome.Carried, "%+v", outcome)
				require.Contains(t, agentSpawn, "--resume "+carryTestID)
				return
			}
			require.False(t, outcome.Carried, "a refused id is not carried")
			require.Equal(t, staleCarryReason, outcome.CarryFailure)
			require.NotContains(t, agentSpawn, "--resume", "the refused id must not be resumed")
			require.NotContains(t, agentSpawn, "--continue", "a refused restore starts fresh, not on Claude's latest")
			requireReplacementRecorded(t, restored, agentSpawn)
		})
	}
}

// #7209 review: a load-time re-spawn (the tmux server died across a reboot)
// refuses a stale Claude id as Recover does, and retires it once the fresh
// spawn commits; left recorded, it would fence every later capture arm and be
// refused again on every restore. A pane that is still alive is reattached and
// keeps its record, since nothing was re-spawned.
func TestLoadRespawnRetiresARefusedClaudeConversation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive bool
	}{{"respawned", false}, {"reattached", true}} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			configDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)

			const agentName = "af_load_claude_stale"
			shellName := agentName + tmuxTabSeparator + shellTabName
			alive := map[string]bool{}
			if tc.alive {
				alive = map[string]bool{agentName: true, shellName: true}
			}
			var newSessions int
			var spawns []string
			exec := recordingExec(alive, &newSessions, &spawns)
			pty := persistPtyFactory{t: t, cmdExec: exec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
			}
			t.Cleanup(func() { restoreTmuxSession = prev })

			data := deadInstanceData(t, Ready, agentName, shellName)
			data.Tabs[0].Conversation = &AgentConversationData{
				Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected,
			}
			project := claudeProjectCandidates(data.Worktree.WorktreePath)[0]
			past := time.Now().Add(-time.Hour)
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, carryTestID+".jsonl"), "{}\n"), past)
			setModTime(t, writeCarryFile(t, configDir, filepath.Join(project, newerConversationID+".jsonl"), "{}\n"), past.Add(time.Minute))

			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			var agentSpawn string
			for _, spawn := range spawns {
				if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellName) {
					agentSpawn = spawn
					break
				}
			}
			if tc.alive {
				require.Empty(t, agentSpawn, "a live pane is reattached, not re-spawned")
				require.Equal(t, carryTestID, restored.AgentConversation().ID)
				return
			}
			require.NotEmpty(t, agentSpawn, "the load must re-spawn the missing agent session")
			require.NotContains(t, agentSpawn, "--resume")
			require.NotContains(t, agentSpawn, "--continue")
			require.NotEqual(t, carryTestID, restored.AgentConversation().ID,
				"the refused id is retired once the fresh spawn commits")
			requireReplacementRecorded(t, restored, agentSpawn)
		})
	}
}

// #7209 review: the load-time retirement parks its pre-spawn capture baseline
// before the runtime generation advances, so the replacement's deferred arm
// can take it. Parked after the advance, it would wait for one that never
// comes, and a rollout Codex minted before the arm would go unrecorded.
func TestLoadRespawnParksACaptureBaselineTheReplacementCanTake(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	const agentName = "af_load_codex_stale"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Ready, agentName, shellName)
	data.Program = tmux.ProgramCodex
	data.Tabs[0].Conversation = &AgentConversationData{
		Agent: tmux.ProgramCodex, ID: carryTestID, CaptureKind: ConversationCaptureCodexRollout,
	}
	worktree := data.Worktree.WorktreePath
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, codexHome, codexRolloutRel(carryTestID), codexRollout(worktree)), past)
	setModTime(t, writeCarryFile(t, codexHome, codexRolloutRel(newerConversationID), codexRollout(worktree)), past.Add(time.Minute))

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.NotEqual(t, carryTestID, restored.AgentConversation().ID, "the stale id is retired")
	park, current := restored.TakePendingConversationCaptureForRuntime(restored.AgentRuntimeToken())
	require.True(t, current)
	require.NotNil(t, park, "the replacement's arm must be able to take the pre-spawn baseline")
}

// requireReplacementRecorded checks that a fresh Claude spawn after a refusal
// was launched on a newly minted session id and that the id is recorded:
// nothing discovers a Claude conversation later, so an unrecorded replacement
// could never be resumed or carried (#7209 review).
func requireReplacementRecorded(t *testing.T, inst *Instance, agentSpawn string) {
	t.Helper()
	recorded := inst.AgentConversation()
	require.True(t, recorded.HasID(), "the replacement's conversation is recorded")
	require.NotEqual(t, carryTestID, recorded.ID)
	require.Equal(t, tmux.ProgramClaude, recorded.Agent)
	require.Equal(t, ConversationCaptureInjected, recorded.CaptureKind)
	require.Contains(t, agentSpawn, "--session-id "+recorded.ID, "the spawn runs the conversation af recorded")
}

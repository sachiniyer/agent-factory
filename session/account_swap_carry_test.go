package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

const carryTestID = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"

// carryAmbientProviderHomes points both providers' ambient stores at HOME, the
// fallback the env model uses, regardless of what the host exports.
func carryAmbientProviderHomes(t *testing.T) string {
	t.Helper()
	home := agentHome(t)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	return home
}

// carrySwapInstance is a limit-blocked, auto-selected session with a real
// worktree and a registered "work" account for agent. It returns the instance
// and the target account's home.
func carrySwapInstance(t *testing.T, agent string) (*Instance, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{agent: agent}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, agent, "work")
	inst := accountSwapTestInstance(agent)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	return inst, target
}

func recordOutgoingConversation(t *testing.T, inst *Instance, agent, id string) {
	t.Helper()
	require.True(t, inst.SetAgentConversation(AgentConversationData{
		Agent: agent, ID: id, CapturedAt: time.Now(), CaptureKind: ConversationCaptureInjected,
	}))
}

func claudeTranscriptRel(worktree, id string) string {
	return filepath.Join("projects", claudeProjectName(worktree), id+".jsonl")
}

func codexRolloutRel(id string) string {
	return filepath.Join("sessions", "2026", "09", "16", "rollout-2026-09-16T01-02-03-"+id+".jsonl")
}

func codexRollout(cwd string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n"+`{"type":"event_msg","payload":{"type":"user_message"}}`+"\n", cwd)
}

func TestPlanAccountSwapCarryAppliesOnlyToSameAgentClaudeAndCodex(t *testing.T) {
	outgoing := AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID}
	for _, tc := range []struct {
		name string
		req  accountSwapCarryRequest
	}{
		{"cross-agent handoff", accountSwapCarryRequest{agent: tmux.ProgramClaude, crossAgent: true, outgoing: outgoing}},
		{"gemini records no addressable conversation", accountSwapCarryRequest{agent: tmux.ProgramGemini, outgoing: outgoing}},
		{"unidentifiable wrapper", accountSwapCarryRequest{agent: "", outgoing: outgoing}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carry, reason := planAccountSwapCarry(tc.req)
			require.Nil(t, carry)
			require.Empty(t, reason, "a swap no carry applies to keeps today's fresh start and its wording")
		})
	}

	for _, tc := range []struct {
		name   string
		req    accountSwapCarryRequest
		reason string
	}{
		{
			name:   "no recorded conversation",
			req:    accountSwapCarryRequest{agent: tmux.ProgramClaude},
			reason: "af had no recorded claude conversation id for the previous session",
		},
		{
			name:   "conversation recorded for another provider",
			req:    accountSwapCarryRequest{agent: tmux.ProgramClaude, outgoing: AgentConversationData{Agent: tmux.ProgramCodex, ID: carryTestID}},
			reason: "af had no recorded claude conversation id for the previous session",
		},
		{
			name:   "an id that cannot name a provider file",
			req:    accountSwapCarryRequest{agent: tmux.ProgramCodex, outgoing: AgentConversationData{Agent: tmux.ProgramCodex, ID: "../auth"}},
			reason: "the recorded conversation id is not a codex session id",
		},
		{
			name:   "a failure this attempt already hit",
			req:    accountSwapCarryRequest{agent: tmux.ProgramClaude, outgoing: outgoing, fallback: "copying it into the new account failed"},
			reason: "copying it into the new account failed",
		},
		{
			name: "a committed fresh transaction never starts a carry",
			req: accountSwapCarryRequest{agent: tmux.ProgramClaude, outgoing: outgoing, target: "work",
				pending: &AccountSwapData{To: "work", ConversationID: carryTestID, CarryFallback: "earlier reason"}},
			reason: "earlier reason",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			carry, reason := planAccountSwapCarry(tc.req)
			require.Nil(t, carry)
			require.Equal(t, tc.reason, reason)
		})
	}
}

func TestValidateAccountSwapCarriesClaudeConversationFromAmbientHome(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramClaude)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	source := filepath.Join(home, ".claude")
	rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
	writeCarryFile(t, source, rel, "{\"type\":\"user\"}\n")
	writeCarryFile(t, source, filepath.Join(filepath.Dir(rel), carryTestID, "tool-results", "one.txt"), "result")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, plan.carry, "a same-agent swap with a recorded conversation must carry it")
	require.Equal(t, source, plan.carry.srcHome)
	require.Equal(t, target, plan.carry.dstHome)
	require.Equal(t, rel, plan.carry.transcript)
	require.Empty(t, plan.carryFallback)
	require.True(t, strings.HasPrefix(plan.program, "claude --resume "+carryTestID+" "), plan.program)
	require.NotContains(t, plan.program, "--session-id", "a carried conversation must never be re-injected as a new one")
	require.Equal(t, AgentConversationData{
		Agent: tmux.ProgramClaude, ID: carryTestID,
		CapturedAt: plan.conversation.CapturedAt, CaptureKind: ConversationCaptureCarried,
	}, plan.conversation)

	generated, ok := sessionenv.GeneratedArgsBetween(plan.base, plan.program)
	require.True(t, ok, "the resume words must be describable as af's own trailing additions")
	require.Equal(t, []string{"--resume", carryTestID}, generated[:2])
	require.Equal(t, generated, plan.proof.GeneratedArgs)
	scope := sessionenv.Account{Agent: tmux.ProgramClaude, Name: "work", Dir: target, GeneratedArgs: plan.proof.GeneratedArgs}
	_, err := sessionenv.ApplyAccount(nil, plan.program, scope)
	require.NoError(t, err, "the account boundary must accept the resume launch it will run")
	require.True(t, inst.PreparedAccountSwapConversation().Carried)

	require.NoFileExists(t, filepath.Join(target, rel), "validation reads the source but copies nothing before teardown")
	require.NoError(t, inst.CarryAccountSwapConversation())
	require.Equal(t, "{\"type\":\"user\"}\n", readCarryFile(t, filepath.Join(target, rel)))
	require.Equal(t, "result", readCarryFile(t,
		filepath.Join(target, filepath.Dir(rel), carryTestID, "tool-results", "one.txt")))

	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, carryTestID, pending.CarriedConversationID)
	require.Empty(t, pending.CarrySourceAccount, "an ambient source is recorded as the empty account")
	require.Empty(t, pending.ConversationID,
		"ConversationID means a fresh injected id; restart recovery would --session-id a carried one")
	require.Equal(t, HandoffConversation{Carried: true}, inst.PendingAccountSwapConversation())
}

func TestValidateAccountSwapCarriesCodexConversationFromRegisteredAccount(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	rel := codexRolloutRel(carryTestID)
	writeCarryFile(t, source, rel, codexRollout(inst.GetWorktreePath()))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, plan.carry)
	require.Equal(t, source, plan.carry.srcHome)
	require.Equal(t, "old", plan.carry.sourceAccount)
	require.Equal(t, rel, plan.carry.transcript, "the rollout keeps its dated layout in the new home")
	require.Equal(t, "codex resume "+carryTestID, plan.program)
	generated, ok := sessionenv.GeneratedArgsBetween(plan.base, plan.program)
	require.True(t, ok)
	require.Equal(t, []string{"resume", carryTestID}, generated)
	require.Equal(t, generated, plan.proof.GeneratedArgs)

	require.NoError(t, inst.CarryAccountSwapConversation())
	require.Equal(t, codexRollout(inst.GetWorktreePath()), readCarryFile(t, filepath.Join(target, rel)))
	require.Equal(t, codexRollout(inst.GetWorktreePath()), readCarryFile(t, filepath.Join(source, rel)))

	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Equal(t, carryTestID, pending.CarriedConversationID)
	require.Equal(t, "old", pending.CarrySourceAccount)

	// The rollout af copied in after the snapshot is not a thread the
	// replacement minted, so capture keeps the carried id.
	conv, err := CaptureAgentConversation(tmux.ProgramCodex, plan.conversationCapture, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, carryTestID, conv.ID)
	require.Equal(t, ConversationCaptureCarried, conv.CaptureKind)
	require.NotEqual(t, plan.conversation.CapturedAt, conv.CapturedAt,
		"an unchanged id must still commit as a new observation, or the caller reads it as a lost runtime")

	// A provider that forked on resume would mint one new rollout for this
	// directory; capture follows it rather than pinning a stale thread.
	const forkID = "0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	writeCarryFile(t, target, codexRolloutRel(forkID), codexRollout(inst.GetWorktreePath()))
	conv, err = CaptureAgentConversation(tmux.ProgramCodex, plan.conversationCapture, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, forkID, conv.ID)
}

func TestValidateAccountSwapStatesWhyItCannotCarry(t *testing.T) {
	carryAmbientProviderHomes(t)
	t.Run("claude transcript missing", func(t *testing.T) {
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)

		require.NoError(t, inst.ValidateAccountSwap("work"))
		plan := inst.accountSwapLaunch
		require.Nil(t, plan.carry)
		require.Equal(t, "its transcript is missing from the previous account's home", plan.carryFallback,
			"a missing artifact must plan the stated fresh start before any runtime stops")
		require.Contains(t, plan.program, "--session-id")
		require.NotContains(t, plan.program, "--resume")
	})
	t.Run("codex rollout missing", func(t *testing.T) {
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, "its rollout is missing from the previous account's home", inst.accountSwapLaunch.carryFallback)
		require.Equal(t, "codex", inst.accountSwapLaunch.program)
	})
	t.Run("codex conversation filed twice in the new account", func(t *testing.T) {
		inst, target := carrySwapInstance(t, tmux.ProgramCodex)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
		writeCarryFile(t, filepath.Join(os.Getenv("HOME"), ".codex"), codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
		writeCarryFile(t, target, filepath.Join("sessions", "2026", "01", "01", "rollout-x-"+carryTestID+".jsonl"), "{}\n")

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, "the new account already files this conversation under a different rollout",
			inst.accountSwapLaunch.carryFallback)
	})
	t.Run("no recorded conversation", func(t *testing.T) {
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, "af had no recorded claude conversation id for the previous session",
			inst.accountSwapLaunch.carryFallback)
	})
}

func TestValidateManualCrossAgentAccountSwapNeverCarries(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramCodex), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides[tmux.ProgramCodex] = tmux.ProgramCodex
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramCodex, "work")
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	writeCarryFile(t, filepath.Join(home, ".claude"), claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")

	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramCodex))
	require.Nil(t, inst.accountSwapLaunch.carry, "providers cannot read each other's transcripts")
	require.Empty(t, inst.accountSwapLaunch.carryFallback, "a cross-agent brief keeps its own wording")
	require.Equal(t, HandoffConversation{}, inst.PreparedAccountSwapConversation())
}

func TestCarryAccountSwapConversationFallsBackToAStatedFreshStart(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramClaude)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
	sourcePath := writeCarryFile(t, filepath.Join(home, ".claude"), rel, "{}\n")
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)

	// The transcript disappears between the preflight check and the copy.
	require.NoError(t, os.Remove(sourcePath))
	require.NoError(t, inst.CarryAccountSwapConversation(), "a failed copy must not abort the swap")

	plan := inst.accountSwapLaunch
	require.Nil(t, plan.carry)
	require.Equal(t, "its transcript is missing from the previous account's home", plan.carryFallback)
	require.Contains(t, plan.program, "--session-id "+plan.conversation.ID)
	require.NotContains(t, plan.program, "--resume")
	require.Equal(t, ConversationCaptureInjected, plan.conversation.CaptureKind)
	require.NoFileExists(t, filepath.Join(target, rel))

	_, err := inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.Empty(t, pending.CarriedConversationID, "pending state must never claim a carry that did not land")
	require.Equal(t, plan.conversation.ID, pending.ConversationID)
	require.Equal(t, plan.carryFallback, pending.CarryFallback)
	require.Equal(t, HandoffConversation{CarryFailure: plan.carryFallback}, inst.PendingAccountSwapConversation())
}

func TestPendingCarriedConversationSurvivesStorageAndReplansAResume(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramClaude)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
	sourcePath := writeCarryFile(t, filepath.Join(home, ".claude"), rel, "{}\n")
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	require.True(t, inst.EndLimitResume())

	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: inst.Path, WorktreePath: inst.Path,
		SessionName: inst.Title, BranchName: "main", ExternalWorktree: true,
	}
	encoded, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"carried_conversation_id":"`+carryTestID+`"`)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	restored, err := FromInstanceData(decoded)
	require.NoError(t, err)
	pending := restored.ToInstanceData().PendingAccountSwap
	require.Equal(t, carryTestID, pending.CarriedConversationID)
	require.Nil(t, pending.OriginalStartupStateUnknown, "the rollback projection must be removed on load")
	require.False(t, restored.AgentConversation().HasID(), "the checkpoint cleared the outgoing slot")

	// The previous account has since lost the transcript; the landed copy is
	// what a committed carry resumes.
	require.NoError(t, os.Remove(sourcePath))
	restored.SetGitWorktreeForTest(inst.gitWorktree)
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	plan := restored.accountSwapLaunch
	require.NotNil(t, plan.carry, "restart recovery must re-plan the carry, not a fresh injection")
	require.True(t, plan.carry.committed)
	require.Equal(t, target, plan.carry.dstHome)
	require.Contains(t, plan.program, "--resume "+carryTestID)
	require.NotContains(t, plan.program, "--session-id",
		"injecting the carried id as a new session would fork or reject the conversation")

	require.NoError(t, restored.SynchronizeAccountSwapRuntimeMetadata())
	conv := restored.AgentConversation()
	require.Equal(t, carryTestID, conv.ID)
	require.Equal(t, ConversationCaptureCarried, conv.CaptureKind)
}

func TestSynchronizeCarriedConversationRefusesAnotherClaudeThread(t *testing.T) {
	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.pendingAccountSwap = &AccountSwapData{To: "work", CarriedConversationID: carryTestID}
	inst.Account = "work"
	require.True(t, inst.SetAgentConversation(AgentConversationData{Agent: tmux.ProgramClaude, ID: "other"}))
	err := inst.SynchronizeAccountSwapRuntimeMetadata()
	require.ErrorContains(t, err, "carried claude conversation")
}

// processSiblingForSwap turns the fixture's shell tab into a process tab, as
// the other respawn tests do: an account-scoped shell needs a system shell
// the host may not provide.
func processSiblingForSwap(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].Command = "git status --short"
	inst.Tabs[1].tmux.SetProgram("git status --short")
}

func TestRespawnForAccountSwapResumesTheCarriedConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	home := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramClaude, "work")

	const agentName = "af_account_swap_carry"
	var newSessions int
	var spawns []string
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	processSiblingForSwap(restored)
	restored.Path = initTempGitRepo(t)
	recordOutgoingConversation(t, restored, tmux.ProgramClaude, carryTestID)
	rel := claudeTranscriptRel(restored.GetWorktreePath(), carryTestID)
	writeCarryFile(t, filepath.Join(home, ".claude"), rel, "{}\n")
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err := restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	// No pre-commit copy ran: the respawn's own idempotent copy is the backstop.
	require.NoError(t, restored.RespawnForAccountSwap())
	require.NotEmpty(t, spawns)
	require.Contains(t, spawns[0], "--resume "+carryTestID)
	require.NotContains(t, spawns[0], "--session-id")
	require.Equal(t, "{}\n", readCarryFile(t, filepath.Join(target, rel)))
	require.Equal(t, carryTestID, restored.AgentConversation().ID)
	require.Equal(t, carryTestID, restored.ToInstanceData().PendingAccountSwap.CarriedConversationID)
}

func TestRespawnForAccountSwapDemotesACarryThatCanNoLongerLand(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual=%v", manual), func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			home := carryAmbientProviderHomes(t)
			bin := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramClaude), []byte("#!/bin/sh\nexit 0\n"), 0o700))
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			cfg := config.DefaultConfig()
			cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
			require.NoError(t, config.SaveConfig(cfg))
			target := registerAccount(t, tmux.ProgramClaude, "work")

			agentName := fmt.Sprintf("af_account_swap_demote_%v", manual)
			var newSessions int
			var spawns []string
			restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
				recordingExec(map[string]bool{}, &newSessions, &spawns))
			processSiblingForSwap(restored)
			restored.Path = initTempGitRepo(t)
			recordOutgoingConversation(t, restored, tmux.ProgramClaude, carryTestID)
			rel := claudeTranscriptRel(restored.GetWorktreePath(), carryTestID)
			sourcePath := writeCarryFile(t, filepath.Join(home, ".claude"), rel, "{}\n")
			restored.SetLimitReached(time.Time{})
			require.NoError(t, restored.BeginLimitResume())
			if manual {
				require.NoError(t, restored.ValidateManualAccountSwap("work", tmux.ProgramClaude))
				require.True(t, restored.PreparedAccountSwapConversation().Carried)
				brief := MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude, Goal: "finish it",
					Conversation: restored.PreparedAccountSwapConversation()}
				_, err := restored.SelectAccountForHandoff("", "work", tmux.ProgramClaude,
					HandoffReasonUsageLimit, "", brief.Render())
				require.NoError(t, err)
			} else {
				require.NoError(t, restored.ValidateAccountSwap("work"))
				_, err := restored.SelectAccountAutomatically("", "work")
				require.NoError(t, err)
			}
			require.Equal(t, carryTestID, restored.ToInstanceData().PendingAccountSwap.CarriedConversationID)

			// The copy never landed and the source is gone: the committed carry
			// cannot be completed, so the launch must say so rather than resume
			// an id the new account does not hold.
			require.NoError(t, os.Remove(sourcePath))
			require.NoError(t, restored.RespawnForAccountSwap())

			require.NotEmpty(t, spawns)
			require.NotContains(t, spawns[0], "--resume")
			require.Contains(t, spawns[0], "--session-id")
			require.NoFileExists(t, filepath.Join(target, rel))
			pending := restored.ToInstanceData().PendingAccountSwap
			require.Empty(t, pending.CarriedConversationID, "pending state must never claim a carried id that was not launched")
			require.Equal(t, "its transcript is missing from the previous account's home", pending.CarryFallback)
			require.NotEmpty(t, pending.ConversationID)
			require.Equal(t, pending.ConversationID, restored.AgentConversation().ID)
			require.Equal(t, ConversationCaptureInjected, restored.AgentConversation().CaptureKind)
			if manual {
				_, mission := restored.PendingManualAccountSwap()
				require.Contains(t, mission, "af tried to carry the previous conversation over to the new account, but its transcript is missing")
				require.NotContains(t, mission, "This conversation continues",
					"the stored mission was rendered for a carry that did not happen")
			}
		})
	}
}

func TestCarriedLaunchesPassTheAccountBoundary(t *testing.T) {
	for _, tc := range []struct {
		agent, want string
	}{
		{tmux.ProgramClaude, "claude --resume " + carryTestID},
		{tmux.ProgramCodex, "codex resume " + carryTestID},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			carry := &conversationCarry{agent: tc.agent, id: carryTestID}
			program, conv, ok := carry.launch(tc.agent)
			require.True(t, ok)
			require.Equal(t, tc.want, program)
			require.Equal(t, ConversationCaptureCarried, conv.CaptureKind)
			proof := accountLaunchProof(tc.agent, program, false)
			require.NotEmpty(t, proof.GeneratedArgs, "the resume words must be declared as af's own")
			dir := filepath.Join(t.TempDir(), tc.agent, "work")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			_, err := sessionenv.ApplyAccount(nil, program, sessionenv.Account{
				Agent: tc.agent, Name: "work", Dir: dir, GeneratedArgs: proof.GeneratedArgs,
			})
			require.NoError(t, err)

			_, err = sessionenv.ApplyAccount(nil, program, sessionenv.Account{Agent: tc.agent, Name: "work", Dir: dir})
			require.Error(t, err, "without af's declaration the same words are unprovable user arguments")
		})
	}
	_, _, ok := (&conversationCarry{agent: tmux.ProgramClaude, id: carryTestID}).launch("claude --continue")
	require.False(t, ok, "a program already pinning a conversation cannot also resume the carried one")
}

func TestPendingCarryFollowsACodexForkForRestartRecovery(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)

	// The replacement resumed onto a new thread rather than appending in place.
	const forkID = "0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	forkRel := filepath.Join("sessions", "2026", "09", "17", "rollout-2026-09-17T00-00-00-"+forkID+".jsonl")
	writeCarryFile(t, target, forkRel, codexRollout(inst.GetWorktreePath()))
	fork := AgentConversationData{Agent: tmux.ProgramCodex, ID: forkID, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout}
	require.True(t, inst.RecordAccountSwapConversationForRuntime(inst.AgentRuntimeToken(), fork))
	require.Equal(t, forkID, inst.AgentConversation().ID)
	require.Equal(t, forkID, inst.ToInstanceData().PendingAccountSwap.CarriedConversationID,
		"restart recovery must resume the thread the replacement is on, not the pre-fork one")

	// A restart that must rebuild the replacement re-plans from the record.
	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry)
	require.Equal(t, forkID, plan.carry.id)
	require.Equal(t, forkRel, plan.carry.transcript, "the fork exists only in the new account, which a committed carry accepts")
	require.Equal(t, "codex resume "+forkID, plan.program)

	stale := AgentRuntimeToken{agent: tmux.ProgramCodex, generation: inst.AgentRuntimeToken().generation + 1}
	require.False(t, inst.RecordAccountSwapConversationForRuntime(stale,
		AgentConversationData{Agent: tmux.ProgramCodex, ID: carryTestID}))
	require.Equal(t, forkID, inst.ToInstanceData().PendingAccountSwap.CarriedConversationID,
		"a capture from a replaced runtime must not move the record")
}

func TestRespawnForAccountSwapDemotesACarryRestartFoundImpossible(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	home := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramClaude, "work")

	const agentName = "af_account_swap_restart_fresh"
	var newSessions int
	var spawns []string
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	processSiblingForSwap(restored)
	restored.Path = initTempGitRepo(t)
	recordOutgoingConversation(t, restored, tmux.ProgramClaude, carryTestID)
	sourcePath := writeCarryFile(t, filepath.Join(home, ".claude"),
		claudeTranscriptRel(restored.GetWorktreePath(), carryTestID), "{}\n")
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err := restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	// The daemon restarts before launching, and by then neither home holds the
	// transcript: recovery validation itself plans the fresh start.
	require.NoError(t, os.Remove(sourcePath))
	require.NoError(t, restored.ValidateAccountSwap("work"))
	require.Nil(t, restored.accountSwapLaunch.carry)
	require.Equal(t, "its transcript is missing from the previous account's home", restored.accountSwapLaunch.carryFallback)

	require.NoError(t, restored.RespawnForAccountSwap())
	require.NotEmpty(t, spawns)
	require.Contains(t, spawns[0], "--session-id")
	pending := restored.ToInstanceData().PendingAccountSwap
	require.Empty(t, pending.CarriedConversationID,
		"a fresh launch must not leave the record claiming the carried id")
	require.Equal(t, "its transcript is missing from the previous account's home", pending.CarryFallback)
	require.Equal(t, restored.AgentConversation().ID, pending.ConversationID)
	require.NoError(t, restored.SynchronizeAccountSwapRuntimeMetadata(),
		"restart metadata sync must accept the fresh id that actually launched")
	require.Equal(t, HandoffConversation{CarryFailure: pending.CarryFallback}, restored.PendingAccountSwapConversation())
}

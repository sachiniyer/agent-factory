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
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/testguard"
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
	// A worktree af created for this session, apart from the repository's own
	// checkout: only there is a newer conversation evidence against the carry
	// (newerConversationIsEvidence, #7227).
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, testguard.CanonicalTempDir(t), inst.Title, "main", "", false, true)
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
	return codexRolloutRelDay(id, time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local))
}

// codexRolloutRelDay is codexRolloutRel under the day dir Codex would use for
// a rollout minted on day. A rollout a live capture must observe must sit in
// the dir the recorder would mint it in now — the bounded scan prunes
// directories provably older than the snapshot window (#6845).
func codexRolloutRelDay(id string, day time.Time) string {
	return filepath.Join("sessions", day.Format("2006"), day.Format("01"), day.Format("02"),
		"rollout-"+day.Format("2006-01-02")+"T01-02-03-"+id+".jsonl")
}

func codexRollout(cwd string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n"+`{"type":"event_msg","payload":{"type":"user_message"}}`+"\n", cwd)
}

// codexRolloutWithSource is codexRollout with a session_meta source: a bare
// string like "exec" for a top-level launch, or an object like
// {"subagent":"review"} for a data-bearing variant (#6766).
func codexRolloutWithSource(cwd, sourceJSON string) string {
	return fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q,"source":%s}}`+"\n"+`{"type":"event_msg","payload":{"type":"user_message"}}`+"\n", cwd, sourceJSON)
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
	require.Equal(t, "codex -c check_for_update_on_startup=false resume "+carryTestID, plan.program)
	generated, ok := sessionenv.GeneratedArgsBetween(plan.base, plan.program)
	require.True(t, ok)
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false", "resume", carryTestID}, generated)
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

	// A sibling rollout that appears during the capture window cannot be
	// distinguished from a real fork by appearance alone, so carried capture
	// fails closed: the carried id stands rather than adopt the sibling's.
	// The sibling must sit in the day dir a real recorder would mint it in —
	// the bounded scan never descends into provably-older directories, so a
	// stale-dated sibling would exercise nothing (#6845).
	const siblingID = "0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	writeCarryFile(t, target, codexRolloutRelDay(siblingID, time.Now()), codexRollout(inst.GetWorktreePath()))
	conv, err = CaptureAgentConversation(tmux.ProgramCodex, plan.conversationCapture, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, carryTestID, conv.ID,
		"a sibling rollout appearing during carried capture must not steal the carried conversation id")
	require.Equal(t, ConversationCaptureCarried, conv.CaptureKind,
		"the carried id is re-stamped as a carried observation, not a rollout capture")
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
		require.Equal(t, "codex -c check_for_update_on_startup=false", inst.accountSwapLaunch.program)
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

	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramCodex, true))
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
				require.NoError(t, restored.ValidateManualAccountSwap("work", tmux.ProgramClaude, false))
				require.True(t, restored.PreparedAccountSwapConversation().Carried)
				brief := MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude, Goal: "finish it",
					Conversation: restored.PreparedAccountSwapConversation()}
				_, err := restored.SelectAccountForHandoff("", "work", tmux.ProgramClaude, tmux.ProgramClaude,
					false, HandoffReasonUsageLimit, "", brief.Render())
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
			proof := accountLaunchProof(tc.agent, program, false, nil, "")
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
	require.Equal(t, "codex -c check_for_update_on_startup=false resume "+forkID, plan.program)

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

// The usage reader quarantines claims below a carried-prefix watermark — the
// records a carry copied hold the SOURCE account's numbers, and without the
// record they would count for the target. The commit half of the copy writes
// it: a codex rollout is proof-safe only once the durable map says which
// bytes are not the target's (#6802 review).
func TestConversationCarryRecordsTheCarriedPrefix(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	content := "claim one\nclaim two\n"
	writeCarryFile(t, srcHome, rel, content)

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy())

	prefixes, err := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
	require.NoError(t, err)
	require.Equal(t, int64(len(content)), prefixes[filepath.Join(dstDir, rel)],
		"every copied byte is the source account's claim — the watermark covers the whole file")
}

// A destination the carry only verifies keeps its own tail past the copied
// prefix: the watermark covers exactly the source-matched bytes, never the
// target's appended records.
func TestConversationCarryWatermarksOnlyTheSourcePrefix(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim one\n")
	dstPath := writeCarryFile(t, dstDir, rel, "claim one\ntarget tail\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy())

	prefixes, err := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
	require.NoError(t, err)
	require.Equal(t, int64(len("claim one\n")), prefixes[dstPath],
		"the verified prefix is the source's; the destination's tail is its own")
}

// A present-but-empty source transcript verifies nothing about a non-empty
// stale destination: the prefix comparison it buys covers zero bytes and
// trivially "matches", so without the carryFileAt guard the stale copy would
// be resumed as the whole conversation with covered == 0 and the carried-
// prefix quarantine (#6802) silently suppressed. The guard mirrors the
// missing-source branch — a new carry refuses, and a committed re-run keeps
// the landed copy but covers every byte as the source account's unverifiable
// provenance so the watermark (not a quarantine marker) holds.
func TestConversationCarryEmptySourceVerifiesNothing(t *testing.T) {
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	stale := "claim one\nclaim two\nrecorded while the previous account was live\n"

	t.Run("a new carry refuses the stale destination", func(t *testing.T) {
		afHome := t.TempDir()
		srcHome := t.TempDir()
		dstDir := filepath.Join(afHome, "accounts", "codex", "work")
		require.NoError(t, os.MkdirAll(dstDir, 0o700))
		dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
		require.NoError(t, err)
		writeCarryFile(t, srcHome, rel, "")
		dstPath := writeCarryFile(t, dstDir, rel, stale)

		carry := &conversationCarry{
			agent: tmux.ProgramCodex, id: "x",
			srcHome: srcHome, dstHome: dstDir,
			srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
			transcript: rel,
		}
		err = carry.copy()
		require.Error(t, err)
		require.Equal(t, carryRolloutEmptyReason, carryFailureReason(err), "a Codex refusal names the rollout")
		require.Equal(t, stale, readCarryFile(t, dstPath),
			"the refused carry must not destroy the destination's existing copy")
		prefixes, perr := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
		require.NoError(t, perr)
		require.Empty(t, prefixes, "no carried-prefix watermark is recorded for a refused carry")
		rails, rerr := handoff.NewRailsStore(
			filepath.Join(afHome, "instances", handoff.RailsFileName),
			filepath.Join(afHome, "instances", handoff.StateFileName)).Load()
		require.NoError(t, rerr)
		require.Empty(t, rails.UnrecordedCarries, "a refused carry commits nothing, so nothing is quarantined")
	})

	t.Run("a committed re-run keeps the stale copy and covers every byte", func(t *testing.T) {
		afHome := t.TempDir()
		srcHome := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(afHome, "instances"), 0o700))
		dstDir := filepath.Join(afHome, "accounts", "codex", "work")
		require.NoError(t, os.MkdirAll(dstDir, 0o700))
		dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
		require.NoError(t, err)
		dstPath := writeCarryFile(t, dstDir, rel, stale)
		writeCarryFile(t, srcHome, rel, "")

		carry := &conversationCarry{
			agent: tmux.ProgramCodex, id: "x",
			srcHome: srcHome, dstHome: dstDir,
			srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
			transcript: rel, committed: true,
		}
		require.NoError(t, carry.copy(),
			"a committed re-run keeps the landed copy rather than refuse the empty source")
		require.Equal(t, stale, readCarryFile(t, dstPath),
			"the stale copy is kept as the conversation to resume")
		prefixes, perr := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
		require.NoError(t, perr)
		require.Equal(t, int64(len(stale)), prefixes[dstPath],
			"every unverifiable byte is the source account's provenance — the watermark covers the whole file")
		rails, rerr := handoff.NewRailsStore(
			filepath.Join(afHome, "instances", handoff.RailsFileName),
			filepath.Join(afHome, "instances", handoff.StateFileName)).Load()
		require.NoError(t, rerr)
		require.NotContains(t, rails.UnrecordedCarries, dstPath,
			"a real watermark covering the whole span needs no quarantine marker")
	})
}

// When the quarantine list is already at its cap, the failed carry cannot
// get a marker — and the cap NEVER evicts one to make room (#6852 review):
// a dropped marker would re-trust a file whose poison span is still on
// disk. The carry instead flags the ACCOUNT home the poison landed in as
// untrusted for retraction: no claim from that account may free a wall
// until a covering watermark retires the flag.
func TestConversationCarryFlagsTheAccountWhenTheQuarantineIsFull(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(afHome, "instances"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(afHome, "instances", "auto-handoff-state.json"),
		[]byte("{not json"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim\n")

	// Seed the rails file at the marker cap — every entry is someone
	// else's poison and must survive.
	full := map[string]any{"schema_version": 1, "unrecorded_carries": map[string]any{}}
	for i := 0; i < 256; i++ {
		full["unrecorded_carries"].(map[string]any)[fmt.Sprintf("/poisoned/%03d", i)] =
			map[string]any{"at": "2026-11-05T18:00:00Z", "covered": 1}
	}
	raw, err := json.Marshal(full)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(
		filepath.Join(afHome, "instances", handoff.RailsFileName), raw, 0o600))

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy(),
		"the copy succeeded — a record-keeping failure must not strand the user's context")
	require.FileExists(t, filepath.Join(dstDir, rel))

	rails, err := handoff.NewRailsStore(
		filepath.Join(afHome, "instances", handoff.RailsFileName),
		filepath.Join(afHome, "instances", handoff.StateFileName)).Load()
	require.NoError(t, err)
	require.Len(t, rails.UnrecordedCarries, 256,
		"the cap never evicts — every standing marker survives the overflow")
	require.NotContains(t, rails.UnrecordedCarries, filepath.Join(dstDir, rel),
		"the overflowed carry got no marker of its own")
	flag, ok := rails.QuarantinedAccounts[dstDir]
	require.True(t, ok, "the overflow must flag the account that took the poison")
	require.Equal(t, filepath.Join(dstDir, rel), flag.Path,
		"the flag names the unrecorded rollout so a covering watermark can retire it")
}

// A corrupt watermark file must never cost the conversation (#6776 review):
// the copy already landed, so the carry COMMITS and the rollout goes into
// the rails file's unrecorded-carry quarantine — every claim in it distrusted
// until a real watermark lands — instead of a fresh start losing the user's
// context.
func TestConversationCarryQuarantinesTheRolloutWhenTheWatermarkCannotBeRecorded(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	// A corrupt state file fails every watermark write AND every state read
	// — but the rails file still takes the quarantine marker.
	require.NoError(t, os.MkdirAll(filepath.Join(afHome, "instances"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(afHome, "instances", "auto-handoff-state.json"),
		[]byte("{not json"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy(),
		"the copy succeeded — a record-keeping failure must not strand the user's context")
	require.FileExists(t, filepath.Join(dstDir, rel),
		"the carried rollout stays behind — it IS the conversation")

	quarantined, err := handoff.NewRailsStore(filepath.Join(afHome, "instances", handoff.RailsFileName), filepath.Join(afHome, "instances", handoff.StateFileName)).Load()
	require.NoError(t, err)
	require.Contains(t, quarantined.UnrecordedCarries, filepath.Join(dstDir, rel),
		"the unrecorded rollout is quarantined end-to-end — no claim in it can free a wall")
}

// When NO durable record can land — the watermark fails AND the
// quarantine write fails — the carry must not commit the copied bytes:
// without a record they would count as the target's own usage claims,
// the silent-poison failure the quarantine exists to prevent (#6852
// round-2 review). The install unwinds and the carry fails, so the swap
// starts a fresh conversation whose brief names why — the same
// fresh-conversation fallback the pre-quarantine behavior had.
func TestConversationCarryUnwindsWhenNoRecordCanBeWritten(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	// instances exists as a FILE, so neither durable write can land
	// beneath it.
	require.NoError(t, os.WriteFile(filepath.Join(afHome, "instances"), []byte("x"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	err = carry.copy()
	require.Error(t, err,
		"with no durable record the copy's claims would count as the target's own — the carry must not commit")
	require.Contains(t, carryFailureReason(err), "watermark")
	require.NoFileExists(t, filepath.Join(dstDir, rel),
		"the unwound install leaves no unrecorded carry bytes behind")
}

// A committed carry that displaces a pre-existing destination restores it
// when the durable record cannot land: the destination goes back to
// exactly what it held before the carry ran.
func TestConversationCarryRestoresTheDisplacedOriginalWhenNoRecordLands(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(afHome, "instances"), []byte("x"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim one\nclaim two\n")
	dstPath := writeCarryFile(t, dstDir, rel, "claim one\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.Error(t, carry.copy())
	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, "claim one\n", string(got),
		"the displaced original is restored — the destination is exactly as it was")
}

// When the watermark write fails and a shorter matching destination was
// displaced, the carried copy still wins: the carry committed, so the kept
// transcript is the source's full content, and the quarantine covers every
// byte in it.
func TestConversationCarryKeepsTheNewCopyWhenTheWatermarkCannotBeRecorded(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(afHome, "instances"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(afHome, "instances", "auto-handoff-state.json"),
		[]byte("{not json"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim one\nclaim two\n")
	dstPath := writeCarryFile(t, dstDir, rel, "claim one\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy())

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, "claim one\nclaim two\n", string(got),
		"the committed carry's full copy stays — the displaced original does not come back over it")
	quarantined, err := handoff.NewRailsStore(filepath.Join(afHome, "instances", handoff.RailsFileName), filepath.Join(afHome, "instances", handoff.StateFileName)).Load()
	require.NoError(t, err)
	require.Contains(t, quarantined.UnrecordedCarries, dstPath)
}

// A re-carry that verifies a SHORTER matching prefix must not retire the
// quarantine the longer failed carry left: its watermark covers only the
// span it copied, and the bytes past it are still the first account's
// unaccounted claims (Codex review on #6852). Only a watermark reaching
// the quarantined span lifts the marker.
func TestConversationCarryKeepsTheQuarantineWhenTheReCarryIsShorter(t *testing.T) {
	afHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(afHome, "instances"), 0o700))
	statePath := filepath.Join(afHome, "instances", handoff.StateFileName)
	require.NoError(t, os.WriteFile(statePath, []byte("{not json"), 0o600))
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	railsStore := func() *handoff.RailsStore {
		return handoff.NewRailsStore(
			filepath.Join(afHome, "instances", handoff.RailsFileName), statePath)
	}
	loadRails := func() handoff.Rails {
		r, err := railsStore().Load()
		require.NoError(t, err)
		return r
	}

	// Carry A installs a long rollout but its watermark cannot land.
	srcA := t.TempDir()
	writeCarryFile(t, srcA, rel, strings.Repeat("a", 1000))
	carryA := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcA, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcA), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carryA.copy())
	marker := loadRails().UnrecordedCarries[filepath.Join(dstDir, rel)]
	require.Equal(t, int64(1000), marker.Covered)

	// The state file recovers; carry B holds only a shorter matching copy.
	require.NoError(t, os.WriteFile(statePath, []byte(`{"schema_version":1}`), 0o600))
	srcB := t.TempDir()
	writeCarryFile(t, srcB, rel, strings.Repeat("a", 500))
	carryB := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcB, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcB), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carryB.copy())

	got, err := os.ReadFile(filepath.Join(dstDir, rel))
	require.NoError(t, err)
	require.Len(t, got, 1000, "the longer destination is kept — carry B verified only its own prefix")
	marker, ok := loadRails().UnrecordedCarries[filepath.Join(dstDir, rel)]
	require.True(t, ok, "a watermark shorter than the quarantined span must not retire the marker")
	require.Equal(t, int64(1000), marker.Covered)
	snap, stateErr, railsErr := handoff.LoadSnapshot(
		handoff.NewStore(statePath), railsStore())
	require.NoError(t, stateErr)
	require.NoError(t, railsErr)
	require.Equal(t, int64(1000), snap.EffectiveCarriedPrefixes()[filepath.Join(dstDir, rel)],
		"the span 500-1000 is still carry A's unaccounted claims")

	// A carry whose watermark reaches the span DOES retire the marker.
	srcC := t.TempDir()
	writeCarryFile(t, srcC, rel, strings.Repeat("a", 1500))
	carryC := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcC, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcC), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carryC.copy())
	require.NotContains(t, loadRails().UnrecordedCarries,
		filepath.Join(dstDir, rel), "a covering watermark retires the marker")
}

// The happy path drops its backup too: a committed replace leaves only the
// carried file, not the displaced original.
func TestConversationCarryDiscardsTheDisplacementBackupOnCommit(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	dstDir := filepath.Join(afHome, "accounts", "codex", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramCodex, "work", dstDir)
	require.NoError(t, err)
	rel := "sessions/2026/11/10/rollout-x.jsonl"
	writeCarryFile(t, srcHome, rel, "claim one\nclaim two\n")
	dstPath := writeCarryFile(t, dstDir, rel, "claim one\n")

	carry := &conversationCarry{
		agent: tmux.ProgramCodex, id: "x",
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: rel,
	}
	require.NoError(t, carry.copy())
	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, "claim one\nclaim two\n", string(got), "the committed carry holds the source's content")
	entries, err := os.ReadDir(filepath.Dir(dstPath))
	require.NoError(t, err)
	require.Len(t, entries, 1, "a committed carry removes the displaced backup")
}

// Claude transcripts carry usage_limit_reached records a limit reader can
// mistake for the target account's own — a claude carry leaves the same
// carried-prefix watermark a codex carry does, so the copied bytes stay the
// SOURCE account's evidence (#6944 review).
func TestConversationCarryRecordsTheCarriedPrefixForClaude(t *testing.T) {
	afHome := t.TempDir()
	srcHome := t.TempDir()
	dstDir := filepath.Join(afHome, "accounts", "claude", "work")
	require.NoError(t, os.MkdirAll(dstDir, 0o700))
	dstRoot, err := accountCarryRoot(afHome, tmux.ProgramClaude, "work", dstDir)
	require.NoError(t, err)
	writeCarryFile(t, srcHome, carryTestRel, "turn one\n")

	carry := &conversationCarry{
		agent: tmux.ProgramClaude, id: carryTestID,
		srcHome: srcHome, dstHome: dstDir,
		srcRoot: ambientCarryRoot(srcHome), dstRoot: dstRoot,
		transcript: carryTestRel,
	}
	require.NoError(t, carry.copy())
	require.FileExists(t, filepath.Join(dstDir, carryTestRel))
	prefixes, err := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName)).CarriedPrefixes()
	require.NoError(t, err)
	require.Equal(t, int64(len("turn one\n")), prefixes[filepath.Join(dstDir, carryTestRel)],
		"the copied transcript bytes are the source account's records")
}

// #6817 review: require_carry's in-memory precondition is admission's own —
// a session with no recorded conversation can never carry, and the provider
// is the launch command's resolution, not the enum.
func TestAccountSwapCarryRecorded(t *testing.T) {
	uuid := "12345678-1234-1234-1234-1234567890ab"
	conv := func(agent, id string) AgentConversationData {
		return AgentConversationData{Agent: agent, ID: id}
	}
	tabs := func(c AgentConversationData) []*Tab {
		return []*Tab{{Name: agentTabName, Kind: TabKindAgent, Conversation: c}}
	}
	cases := []struct {
		name string
		inst *Instance
		want bool
	}{
		{"codex session, recorded codex conversation", &Instance{
			Program: tmux.ProgramCodex, runtimeProgram: "codex",
			Tabs: tabs(conv(tmux.ProgramCodex, uuid)),
		}, true},
		{"codex session, no recorded conversation", &Instance{
			Program: tmux.ProgramCodex, runtimeProgram: "codex",
			Tabs: tabs(AgentConversationData{}),
		}, false},
		{"conversation belongs to another provider", &Instance{
			Program: tmux.ProgramCodex, runtimeProgram: "codex",
			Tabs: tabs(conv(tmux.ProgramClaude, uuid)),
		}, false},
		{"override launching codex under the claude enum", &Instance{
			Program: tmux.ProgramClaude, runtimeProgram: "/opt/bin/codex --full-auto",
			Tabs: tabs(conv(tmux.ProgramCodex, uuid)),
		}, true},
		{"override launching a plain shell is not carry-capable", &Instance{
			Program: tmux.ProgramCodex, runtimeProgram: "./bin/sidekick",
			Tabs: tabs(conv(tmux.ProgramCodex, uuid)),
		}, false},
		{"gemini records no addressable conversation", &Instance{
			Program: tmux.ProgramGemini, runtimeProgram: "gemini",
			Tabs: tabs(conv(tmux.ProgramGemini, uuid)),
		}, false},
	}
	for _, tc := range cases {
		if got := tc.inst.AccountSwapCarryRecorded(); got != tc.want {
			t.Fatalf("%s: AccountSwapCarryRecorded() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// #6817 review: the codex-home judgment follows the launch command, not the
// enum — a program_overrides session running codex under another name still
// writes a codex home worth reading.
func TestSessionCodexHomeDirIdentifiesCodexFromTheLaunchCommand(t *testing.T) {
	home := t.TempDir()
	repo := initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, "t", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Program:        tmux.ProgramClaude, // enum lies: the launch runs codex
		Path:           repo,
		runtimeProgram: "env CODEX_HOME=" + home + " codex",
	}
	inst.SetGitWorktreeForTest(gw)
	if got := inst.SessionCodexHomeDir(); got != home {
		t.Fatalf("SessionCodexHomeDir() = %q, want %q", got, home)
	}
	// And the mirror: enum codex whose launch command is a plain shell has
	// no codex home to read.
	shell := &Instance{
		Program:        tmux.ProgramCodex,
		Path:           repo,
		runtimeProgram: "./bin/sidekick",
	}
	shell.SetGitWorktreeForTest(gw)
	if got := shell.SessionCodexHomeDir(); got != "" {
		t.Fatalf("shell-launched session resolved a codex home: %q", got)
	}
}

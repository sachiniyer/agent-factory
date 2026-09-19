package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// Review follow-ups for #4504: a carry must resume the conversation the user is
// actually in, survive its source account being unregistered once committed,
// give up on a resume the new account could not run, and say which symlink it
// refused to follow.

const newerConversationID = "9c8b7a6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"

func setModTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, at, at))
}

func TestCarryRefusesARecordedConversationThatIsNoLongerTheNewest(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	t.Run("claude after /clear", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		source := filepath.Join(home, ".claude")
		recorded := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
		newer := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), newerConversationID), "{}\n")
		setModTime(t, recorded, past)
		setModTime(t, newer, past.Add(time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry,
			"resuming the conversation the user left with /clear would continue abandoned context")
		require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
		require.Contains(t, inst.accountSwapLaunch.program, "--session-id")
	})
	t.Run("claude whose recorded conversation is the newest", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		source := filepath.Join(home, ".claude")
		older := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), newerConversationID), "{}\n")
		recorded := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
		setModTime(t, older, past)
		setModTime(t, recorded, past.Add(time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.NotNil(t, inst.accountSwapLaunch.carry, "an older conversation in the same project does not block the carry")
	})
	t.Run("codex after /new", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
		source := filepath.Join(home, ".codex")
		recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
		newer := writeCarryFile(t, source, codexRolloutRel(newerConversationID), codexRollout(inst.GetWorktreePath()))
		setModTime(t, recorded, past)
		setModTime(t, newer, past.Add(time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
	})
	t.Run("codex with a newer rollout for another worktree", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
		source := filepath.Join(home, ".codex")
		recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
		other := writeCarryFile(t, source, codexRolloutRel(newerConversationID), codexRollout(t.TempDir()))
		setModTime(t, recorded, past)
		setModTime(t, other, past.Add(time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.NotNil(t, inst.accountSwapLaunch.carry,
			"a shared Codex home holds other worktrees' rollouts; only this worktree's newer threads matter")
	})
}

func TestCommittedCarrySurvivesItsSourceAccountBeingUnregistered(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramClaude)
	source := registerAccount(t, tmux.ProgramClaude, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
	writeCarryFile(t, source, rel, "{}\n")
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)

	// The previous account is removed after the copy landed in the new one.
	require.NoError(t, os.RemoveAll(source))
	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry, "the committed carry's copy already lives in the new account")
	require.True(t, plan.carry.committed)
	require.Equal(t, rel, plan.carry.transcript)
	require.Contains(t, plan.program, "--resume "+carryTestID)
	require.NoError(t, plan.carry.copy(), "a gone source is not a failure once the destination holds the copy")
	require.FileExists(t, filepath.Join(target, rel))

	_, err = inst.ensureAccountSwapConversationCarried(cloneAccountSwapLaunchPlan(plan))
	require.NoError(t, err)
	require.Equal(t, carryTestID, inst.ToInstanceData().PendingAccountSwap.CarriedConversationID)

	// Without the landed copy there is nothing left to resume, and the reason
	// says why the source could not supply it.
	require.NoError(t, os.Remove(filepath.Join(target, rel)))
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.Nil(t, inst.accountSwapLaunch.carry)
	require.Equal(t, `the previous account "old" is no longer registered for claude`, inst.accountSwapLaunch.carryFallback)
}

func TestAbandonCarriedConversationAfterAFailedCarriedLaunch(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	home := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramClaude, "work")

	const agentName = "af_account_swap_carry_launch_fails"
	const processName = agentName + "__build"
	var newSessions int
	executor := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName, executor)
	processSiblingForSwap(restored)
	restored.mu.Lock()
	restored.Tabs = append(restored.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess, Command: "git status --short",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(processName, "git status --short",
			failAccountSwapProcessPty{t: t, cmdExec: executor, name: processName}, executor),
	})
	restored.mu.Unlock()
	restored.Path = initTempGitRepo(t)
	recordOutgoingConversation(t, restored, tmux.ProgramClaude, carryTestID)
	writeCarryFile(t, filepath.Join(home, ".claude"), claudeTranscriptRel(restored.GetWorktreePath(), carryTestID), "{}\n")
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err := restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	// Nothing has launched yet: a retry keeps the carry.
	require.NoError(t, restored.AbandonCarriedConversationAfterFailedLaunch("work"))
	require.Equal(t, carryTestID, restored.ToInstanceData().PendingAccountSwap.CarriedConversationID,
		"a committed carry that never launched is not a failed carry")

	require.Error(t, restored.RespawnForAccountSwap(), "the fixture's sibling pane refuses to start")
	require.True(t, restored.ToInstanceData().PendingAccountSwap.CarriedLaunchStarted)

	require.NoError(t, restored.AbandonCarriedConversationAfterFailedLaunch("work"))
	pending := restored.ToInstanceData().PendingAccountSwap
	require.Empty(t, pending.CarriedConversationID, "every retry would re-plan the same resume")
	require.Equal(t, abandonedCarryReason, pending.CarryFallback)
	require.NotEmpty(t, pending.ConversationID)
	require.False(t, pending.CarriedLaunchStarted)

	require.NoError(t, restored.ValidateAccountSwap("work"))
	plan := restored.accountSwapLaunch
	require.Nil(t, plan.carry)
	require.Contains(t, plan.program, "--session-id "+pending.ConversationID)
	require.NotContains(t, plan.program, "--resume")
	require.Equal(t, HandoffConversation{CarryFailure: abandonedCarryReason}, restored.PendingAccountSwapConversation())
}

func TestCarryNamesTheSymlinkItRefusesToFollow(t *testing.T) {
	t.Run("claude projects directory", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		elsewhere := t.TempDir()
		writeCarryFile(t, elsewhere, strings.TrimPrefix(claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "projects/"), "{}\n")
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o700))
		link := filepath.Join(home, ".claude", "projects")
		require.NoError(t, os.Symlink(elsewhere, link))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, link+" is a symbolic link, which af does not follow when carrying a conversation",
			inst.accountSwapLaunch.carryFallback, "a symlinked store is not a missing transcript")
	})
	t.Run("codex sessions directory", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
		elsewhere := t.TempDir()
		writeCarryFile(t, elsewhere, strings.TrimPrefix(codexRolloutRel(carryTestID), "sessions/"), codexRollout(inst.GetWorktreePath()))
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
		link := filepath.Join(home, ".codex", "sessions")
		require.NoError(t, os.Symlink(elsewhere, link))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		require.Equal(t, link+" is a symbolic link, which af does not follow when carrying a conversation",
			inst.accountSwapLaunch.carryFallback)
	})
	t.Run("copy through a symlinked directory", func(t *testing.T) {
		src, dst := carryHomes(t)
		elsewhere := t.TempDir()
		writeCarryFile(t, elsewhere, "-repo/5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d.jsonl", "turn one\n")
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(src, "projects")))
		err := carryConversationFile(ambientCarryRoot(src), plainCarryRoot(dst), carryTestRel, true)
		require.Error(t, err)
		require.Equal(t, filepath.Join(src, "projects")+" is a symbolic link, which af does not follow when carrying a conversation",
			carryFailureReason(err))
	})
}

package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7140 F8: the daemon classifies an account swap's capture — carried or
// fresh — from the snapshot it discovers against. The respawn re-runs a
// committed carry's copy, and a copy that can no longer land demotes the plan
// to a fresh start with its own before-image. The snapshot that describes the
// launched runtime is therefore the one the respawn launched with, not the one
// admission froze before it.

// lostCodexInstanceForRecover is lostInstanceForRecover for a Codex row.
func lostCodexInstanceForRecover(t *testing.T, agentName, shellName string, newSessions *int, spawns *[]string) *Instance {
	t.Helper()
	exec := recordingExec(map[string]bool{}, newSessions, spawns)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	data.Program = tmux.ProgramCodex
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0o755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	return restored
}

// committedCodexCarry commits an automatic swap to "work" that carries the
// outgoing Codex thread, with no pre-commit copy, so the respawn's own copy is
// the first. It returns the row and the outgoing rollout the copy reads.
func committedCodexCarry(t *testing.T, agentName string, spawns *[]string) (*Instance, string) {
	t.Helper()
	home := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: tmux.ProgramCodex}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramCodex, "work")

	var newSessions int
	restored := lostCodexInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName, &newSessions, spawns)
	processSiblingForSwap(restored)
	restored.Path = initTempGitRepo(t)
	recordOutgoingConversation(t, restored, tmux.ProgramCodex, carryTestID)
	source := writeCarryFile(t, filepath.Join(home, ".codex"), codexRolloutRel(carryTestID),
		codexRollout(restored.GetWorktreePath()))
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err := restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	admitted, err := restored.AccountSwapConversationCapture()
	require.NoError(t, err)
	require.True(t, admitted.CarriesConversation(), "fixture: admission planned a carry")
	return restored, source
}

func TestRespawnForAccountSwapReturnsTheCaptureOfThePlanThatLaunched(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	t.Run("a carry demoted at the respawn launches fresh", func(t *testing.T) {
		var spawns []string
		restored, source := committedCodexCarry(t, "af_swap_capture_demoted", &spawns)
		// The copy never landed and the source is gone, so the respawn demotes.
		require.NoError(t, os.Remove(source))

		launched, err := restored.RespawnForAccountSwapWithLiveBoundary(nil)
		require.NoError(t, err)
		require.NotEmpty(t, spawns)
		require.NotContains(t, spawns[0], "resume "+carryTestID, "fixture: the replacement launched fresh")
		require.Empty(t, restored.ToInstanceData().PendingAccountSwap.CarriedConversationID)

		require.False(t, launched.CarriesConversation(),
			"the replacement launched fresh, so its capture must not confirm the carried thread or refuse as a carried one")
		require.True(t, launched.CapturesCodex())
		require.NoError(t, launched.CodexBaselineErr())
	})

	t.Run("a carry that lands still carries", func(t *testing.T) {
		var spawns []string
		restored, _ := committedCodexCarry(t, "af_swap_capture_carried", &spawns)

		launched, err := restored.RespawnForAccountSwapWithLiveBoundary(nil)
		require.NoError(t, err)
		require.NotEmpty(t, spawns)
		require.Contains(t, spawns[0], "resume "+carryTestID, "fixture: the replacement resumed the carried thread")
		require.True(t, launched.CarriesConversation())
	})
}

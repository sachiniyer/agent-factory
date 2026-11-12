package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7126 suite, session half. A store the capture cannot read is a capture
// channel that is unavailable, not an answer about which thread the runtime is
// on: a fresh account swap must be able to tell it apart from every other
// capture failure so it can defer instead of failing a committed handoff.

// blockCodexDay makes one day directory inside the scan window unreadable, the
// shape a transient EACCES/EIO on the account's store takes.
func blockCodexDay(t *testing.T, codexHome string, day time.Time) {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestCaptureAgentConversation_AnUnreadableStoreIsAnUnavailableChannel(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	t.Run("the baseline could not be taken", func(t *testing.T) {
		codexHome := t.TempDir()
		blockCodexDay(t, codexHome, yesterday)
		snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, t.TempDir())
		require.Error(t, snap.CodexBaselineErr(), "fixture: the baseline must fail")

		_, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
		require.ErrorIs(t, err, ErrCodexCaptureChannelUnavailable)
	})
	t.Run("the store became unreadable after the baseline", func(t *testing.T) {
		codexHome := t.TempDir()
		snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, t.TempDir())
		require.NoError(t, snap.CodexBaselineErr())
		blockCodexDay(t, codexHome, yesterday)

		_, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
		require.ErrorIs(t, err, ErrCodexCaptureChannelUnavailable)
	})
}

// TestCaptureAgentConversation_ACarriedConfirmationStillRefusesAnUnreadableStore
// is the guard: a carried swap's capture deliberately refuses a store it cannot
// enumerate, because returning the carried id would hide a broken store behind
// a false "conversation confirmed". #7126 leaves that refusal exactly as it is.
func TestCaptureAgentConversation_ACarriedConfirmationStillRefusesAnUnreadableStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()
	workDir := t.TempDir()
	carriedPath := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.ExpectCarriedCodexRolloutForTest(AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldTestOwnID, CaptureKind: ConversationCaptureCodexRollout,
	}, carriedPath)
	require.True(t, snap.CarriesConversation())
	blockCodexDay(t, codexHome, time.Now().AddDate(0, 0, -1))

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
	require.Error(t, err, "a carried confirmation must not hide a broken store")
	require.False(t, conv.HasID())
}

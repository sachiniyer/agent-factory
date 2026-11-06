package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestCaptureAgentConversation_CodexSiblingRolloutIsNotAdopted is the #6847
// defect: a live, unrecorded session's capture sees a lone new rollout minted
// in the launch working directory — by a sibling Codex sharing the worktree
// and the account-scoped store — while the session's own runtime has written
// nothing. Appearance is directory evidence, not process evidence; recording
// it would make the next restore resume the sibling's thread. The session
// must stay unrecorded.
func TestCaptureAgentConversation_CodexSiblingRolloutIsNotAdopted(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
	require.NoError(t, err)
	require.False(t, conv.HasID(),
		"a rollout not positively attributed to the session's own runtime must not be recorded as its conversation")
}

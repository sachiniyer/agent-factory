package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func writeCodexRolloutFile(t *testing.T, codexHome, name string) string {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0644))
	return path
}

func TestCaptureAgentConversation_CodexRolloutFile(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-33-aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa.jsonl")

	snap := BeginConversationCapture()
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, time.Second)
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, conv.Agent)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
	require.Equal(t, ConversationCaptureCodexRollout, conv.CaptureKind)
	require.False(t, conv.CapturedAt.IsZero())
}

func TestCaptureAgentConversation_CodexAmbiguousConcurrentRollouts(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()

	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl")

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, time.Second)
	require.Error(t, err)
	require.False(t, conv.HasID(), "ambiguous capture must not guess a conversation id")
}

func TestCaptureAgentConversation_UnsupportedAgentGracefullyNoID(t *testing.T) {
	snap := BeginConversationCapture()
	conv, err := CaptureAgentConversation(tmux.ProgramGemini, snap, 0)
	require.NoError(t, err)
	require.False(t, conv.HasID())
}

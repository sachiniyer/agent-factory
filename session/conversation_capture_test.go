package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
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

func TestBeginConversationCaptureAtCodexHomeIgnoresDaemonEnvironment(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	exactHome := t.TempDir()
	snap := BeginConversationCaptureAtCodexHome(exactHome)
	require.Equal(t, exactHome, snap.codexHome)

	path := writeCodexRolloutFile(t, exactHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "inline-home briefing"},
	})
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "inline-home briefing", 0))
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
	for _, agent := range []string{tmux.ProgramGemini, tmux.ProgramAmp} {
		conv, err := CaptureAgentConversation(agent, snap, 0)
		require.NoError(t, err)
		require.False(t, conv.HasID())
	}
}

func appendCodexRolloutEvent(t *testing.T, path string, event any) {
	t.Helper()
	data, err := json.Marshal(event)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.Write(append(data, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestWaitForPromptReceipt_CodexUserMessage(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	const prompt = "the exact config-agent briefing"
	appendCodexRolloutEvent(t, path, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":    "user_message",
			"message": prompt,
		},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, time.Second))
}

func TestWaitForPromptReceipt_SessionMetaIsNotAReceipt(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "briefing never submitted", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"a created Codex session is not proof that its composer accepted a user turn")
}

func TestWaitForPromptReceipt_DifferentUserTurnDoesNotConfirmBriefing(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	appendCodexRolloutEvent(t, path, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "some other prompt",
			}},
		},
	})

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"a concurrent/unrelated user turn must not acknowledge the config briefing")
}

func TestWaitForPromptReceipt_ConcurrentRolloutsRemainAmbiguous(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	other := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	want := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl")
	appendCodexRolloutEvent(t, other, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "unrelated session"},
	})
	appendCodexRolloutEvent(t, want, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptAmbiguous,
		"prompt coincidence cannot prove which new rollout belongs to the launched pane")
}

func TestWaitForPromptReceipt_UnsupportedAgentDoesNotInventReceipt(t *testing.T) {
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramClaude, ConversationCaptureSnapshot{}, "briefing", 0)
	require.True(t, errors.Is(err, ErrPromptReceiptUnavailable))
}

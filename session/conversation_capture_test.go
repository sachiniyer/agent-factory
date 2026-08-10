package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
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

func writeCodexRolloutFileWithCwd(t *testing.T, codexHome, name, cwd string) string {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": cwd},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0644))
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

func TestCaptureAgentConversation_CodexConcurrentRolloutsCorrelateByCwd(t *testing.T) {
	codexHome := t.TempDir()
	workDirA := filepath.Join(t.TempDir(), "session-a")
	workDirB := filepath.Join(t.TempDir(), "session-b")
	snapA := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDirA)
	snapB := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDirB)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDirA)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDirB)

	convA, err := CaptureAgentConversation(tmux.ProgramCodex, snapA, 0)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", convA.ID)

	convB, err := CaptureAgentConversation(tmux.ProgramCodex, snapB, 0)
	require.NoError(t, err)
	require.Equal(t, "019f386f-75b6-7f68-88e3-6d5e1f15bb6a", convB.ID)
}

func TestCaptureAgentConversation_CodexSameCwdRemainsAmbiguous(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.ErrorContains(t, err, "2 new rollout files matched launch working directory")
	require.False(t, conv.HasID(), "same-cwd rollouts cannot be distinguished safely")
}

func TestCaptureAgentConversation_CodexUncorrelatableMetadataDoesNotGuess(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl")

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.ErrorContains(t, err, "1 had uncorrelatable session metadata")
	require.False(t, conv.HasID(), "unknown metadata could still belong to the launch cwd")
}

func TestCaptureAgentConversation_CodexRetriesIncompleteConcurrentMetadata(t *testing.T) {
	codexHome := t.TempDir()
	workDirA := filepath.Join(t.TempDir(), "session-a")
	workDirB := filepath.Join(t.TempDir(), "session-b")
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDirA)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDirA)
	incomplete := writeCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl")
	require.NoError(t, os.WriteFile(incomplete, []byte(`{"type":"session_meta","payload":`), 0644))

	conv, retry, err := captureCodexConversationOnce(snap)
	require.ErrorContains(t, err, "capture undecided")
	require.True(t, retry, "a partial concurrent header can become classifiable on the next poll")
	require.False(t, conv.HasID())

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDirB)
	conv, err = CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
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

func TestWaitForPromptReceipt_RejectsNamedPipeWithoutBlocking(t *testing.T) {
	codexHome := t.TempDir()
	snap := BeginConversationCaptureAtCodexHome(codexHome)
	rolloutDir := filepath.Join(codexHome, "sessions", "2026", "07", "30")
	require.NoError(t, os.MkdirAll(rolloutDir, 0755))
	fifo := filepath.Join(rolloutDir,
		"rollout-2026-07-30T12-00-00-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	require.NoError(t, syscall.Mkfifo(fifo, 0600))

	done := make(chan error, 1)
	go func() {
		done <- WaitForPromptReceipt(
			context.Background(), tmux.ProgramCodex, snap, "briefing", 100*time.Millisecond)
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorContains(t, err, "not a regular file")
	case <-time.After(time.Second):
		fd, unblockErr := syscall.Open(
			fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if unblockErr == nil {
			require.NoError(t, syscall.Close(fd))
		}
		select {
		case eventualErr := <-done:
			t.Fatalf("HUNG: the 100ms prompt-receipt window blocked opening rollout FIFO %s; "+
				"after bounded cleanup supplied a writer, it returned %v", fifo, eventualErr)
		case <-time.After(time.Second):
			t.Fatalf("HUNG: the prompt-receipt reader did not return after bounded "+
				"nonblocking FIFO cleanup (%v)", unblockErr)
		}
	}
}

func TestWaitForPromptReceipt_UnsupportedAgentDoesNotInventReceipt(t *testing.T) {
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramClaude, ConversationCaptureSnapshot{}, "briefing", 0)
	require.True(t, errors.Is(err, ErrPromptReceiptUnavailable))
}

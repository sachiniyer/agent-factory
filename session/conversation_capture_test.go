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

func writeEmptyCodexRolloutFile(t *testing.T, codexHome, name string) string {
	t.Helper()
	// Same directory as writeCodexRolloutFile so a later write to the same name
	// settles THIS file rather than creating a sibling.
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, nil, 0644))
	return path
}

type captureResult struct {
	conv AgentConversationData
	err  error
}

// TestCaptureAgentConversation_CodexSessionMetaSettlesAfterBaseDeadline is the
// #3266 production failure: the rollout file appears inside the capture window,
// but Codex writes its session_meta header ~2.6–2.8s after process start, past
// the base timeout. Expiry while a concrete candidate is still unclassifiable
// is not absence; capture must keep polling until the header settles.
func TestCaptureAgentConversation_CodexSessionMetaSettlesAfterBaseDeadline(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	const name = "rollout-2026-08-13T01-56-28-019ffa56-4ff5-7f81-b31f-36d3fc702b3e.jsonl"
	writeEmptyCodexRolloutFile(t, codexHome, name)

	done := make(chan captureResult, 1)
	go func() {
		conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
		done <- captureResult{conv, err}
	}()

	time.Sleep(800 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("capture gave up while the sole rollout header was still settling: conv=%+v err=%v", r.conv, r.err)
	default:
	}

	writeCodexRolloutFileWithCwd(t, codexHome, name, workDir)
	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, "019ffa56-4ff5-7f81-b31f-36d3fc702b3e", r.conv.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not resolve after the rollout header settled")
	}
}

// TestCaptureAgentConversation_CodexUndecidedSettleWindowIsBounded: the settle
// extension waits for evidence that already exists, not forever. A header that
// never becomes classifiable still ends in the undecided refusal.
func TestCaptureAgentConversation_CodexUndecidedSettleWindowIsBounded(t *testing.T) {
	oldSettle := codexSessionMetaSettleTimeout
	codexSessionMetaSettleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { codexSessionMetaSettleTimeout = oldSettle })

	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	writeEmptyCodexRolloutFile(t, codexHome,
		"rollout-2026-08-13T01-56-28-019ffa56-4ff5-7f81-b31f-36d3fc702b3e.jsonl")

	start := time.Now()
	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 50*time.Millisecond)
	require.ErrorContains(t, err, "uncorrelatable session metadata")
	require.False(t, conv.HasID(), "a header that never settled cannot be correlated")
	require.Less(t, time.Since(start), 5*time.Second, "the settle extension must stay bounded")
}

// TestCaptureAgentConversation_CodexSettleWindowKeepsAmbiguityRefusal: evidence
// that settles DURING the extension is classified by the same fail-closed rules.
// Two rollouts naming the launch cwd refuse, exactly as inside the base window.
func TestCaptureAgentConversation_CodexSettleWindowKeepsAmbiguityRefusal(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	const first = "rollout-2026-08-13T01-56-28-019ffa56-4ff5-7f81-b31f-36d3fc702b3e.jsonl"
	writeEmptyCodexRolloutFile(t, codexHome, first)

	done := make(chan captureResult, 1)
	go func() {
		conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
		done <- captureResult{conv, err}
	}()

	time.Sleep(400 * time.Millisecond)
	// A same-cwd sibling settles first; the poll must stay undecided while the
	// first header is still unclassifiable rather than capture the sibling.
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-13T01-56-29-019ffa5a-ddb3-7941-8cad-7c688ceb97d4.jsonl", workDir)
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("capture must not decide while a candidate header is still settling: conv=%+v err=%v", r.conv, r.err)
	default:
	}
	writeCodexRolloutFileWithCwd(t, codexHome, first, workDir)

	select {
	case r := <-done:
		require.ErrorContains(t, r.err, "matched launch working directory")
		require.False(t, r.conv.HasID(), "same-cwd rollouts cannot be distinguished safely")
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not resolve after both headers settled")
	}
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

// TestWaitForPromptReceipt_TrailingWhitespacePromptMatchesStrippedTurn is the
// #5488 production failure: the config-agent briefing ends in a newline and
// Codex strips it when recording the submitted turn, so the verbatim prompt
// was never contained in its own receipt. The fixture below is a real
// response_item turn as Codex 0.164 records it — identical to the prompt
// except rstripped.
func TestWaitForPromptReceipt_TrailingWhitespacePromptMatchesStrippedTurn(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	const prompt = "the config-agent briefing\nwith several lines\n\n"
	appendCodexRolloutEvent(t, path, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				// Codex strips trailing whitespace when recording the turn.
				"text": "the config-agent briefing\nwith several lines",
			}},
		},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0),
		"a turn recorded as prompt.rstrip() is still the prompt's receipt")
}

// A prompt with no attributable text can never be proven in the rollout —
// refusing it as undecidable keeps an empty needle from matching every
// recorded turn (#5488).
func TestWaitForPromptReceipt_WhitespaceOnlyPromptIsUndecidable(t *testing.T) {
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
				"text": "a recorded turn that must not satisfy a whitespace prompt",
			}},
		},
	})

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, " \n\t", 0)
	require.ErrorIs(t, err, ErrPromptReceiptUndecidable,
		"a whitespace-only prompt must not become a match-everything empty needle")
}

// A lone prompt match is not yet unique attribution while another new
// rollout's header is still unclassifiable: that file could settle into a
// second match, so the poll must wait rather than confirm early (#5471
// review).
func TestCodexPromptReceiptOnce_LoneMatchWaitsForUnsettledRollout(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	const prompt = "the exact config-agent briefing"
	matched := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-13T01-56-28-019ffa56-4ff5-7f81-b31f-36d3fc702b3e.jsonl", workDir)
	appendCodexRolloutEvent(t, matched, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	const settling = "rollout-2026-08-13T01-56-30-019ffa5a-ddb3-7941-8cad-7c688ceb97d4.jsonl"
	writeEmptyCodexRolloutFile(t, codexHome, settling)

	received, undecided, err := codexPromptReceiptOnce(snap, prompt)
	require.NoError(t, err)
	require.False(t, received, "a still-unclassifiable rollout could settle into a second match")
	require.True(t, undecided)

	// Once it settles into another working directory the match is unique
	// and the receipt confirms.
	writeCodexRolloutFileWithCwd(t, codexHome, settling, t.TempDir())
	received, undecided, err = codexPromptReceiptOnce(snap, prompt)
	require.NoError(t, err)
	require.True(t, received)
	require.False(t, undecided)
}

func TestWaitForPromptReceipt_ConcurrentRolloutsRemainAmbiguous(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	other := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	want := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl")
	for _, path := range []string{other, want} {
		appendCodexRolloutEvent(t, path, map[string]any{
			"type":    "event_msg",
			"payload": map[string]any{"type": "user_message", "message": "config briefing"},
		})
	}

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptAmbiguous,
		"two rollouts recording the same briefing cannot prove which belongs to the launched pane")
}

// The receipt question is "was the briefing observed", not "which file is the
// session's": among several new rollouts, the one that recorded the prompt is
// the receipt, and the rest are ignored (#5471 review).
func TestWaitForPromptReceipt_OneMatchAmongCandidatesIsTheReceipt(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	other := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	mine := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl")
	appendCodexRolloutEvent(t, other, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "unrelated session"},
	})
	appendCodexRolloutEvent(t, mine, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0),
		"exactly one matching rollout is the receipt, regardless of how many candidates exist")
}

// A bound snapshot correlates the receipt to the launch working directory —
// the rule capture already applies (#2228 review) and the one that keeps the
// config agent's 5-minute late re-check answerable on a box where other Codex
// sessions create rollouts in the same CODEX_HOME (#5471 review).
func TestWaitForPromptReceipt_WorkingDirCorrelationIgnoresOtherCwds(t *testing.T) {
	codexHome := t.TempDir()
	workDirA := filepath.Join(t.TempDir(), "session-a")
	workDirB := filepath.Join(t.TempDir(), "session-b")
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDirA)

	other := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDirB)
	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDirA)
	appendCodexRolloutEvent(t, other, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "unrelated session"},
	})
	appendCodexRolloutEvent(t, mine, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0),
		"a sibling Codex's rollout in another working directory must not force ambiguity")
}

func TestWaitForPromptReceipt_WorkingDirSameCwdStaysAmbiguous(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	for _, name := range []string{
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl",
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl",
	} {
		path := writeCodexRolloutFileWithCwd(t, codexHome, name, workDir)
		appendCodexRolloutEvent(t, path, map[string]any{
			"type":    "event_msg",
			"payload": map[string]any{"type": "user_message", "message": "config briefing"},
		})
	}

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptAmbiguous,
		"two same-directory rollouts both recording the briefing cannot be told apart")
}

// A new rollout whose session_meta is still mid-write cannot be ruled OUT as
// the awaited one, so expiry under it is "undecidable", never "not observed" —
// the late re-check's never-arrived verdict must not claim a file it could not
// classify (#5471), and the error is typed so unconfirmed-outcome callers keep
// observing rather than dropping the diagnostic watcher.
func TestWaitForPromptReceipt_UncorrelatableRolloutStaysUndecided(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	writeCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptUndecidable)
	require.NotErrorIs(t, err, ErrPromptReceiptNotObserved,
		"an unclassifiable rollout is not absence")
}

// Two config agents launch into the same AF home, so a sibling rollout CAN
// appear mid-wait inside the working-directory correlation (#5471 review).
// Attribution is the recorded turn itself: the sibling's own unrelated turns
// are not the awaited prompt, so its presence changes nothing about the scan
// and the receipt still resolves on the rollout that did record it.
func TestWaitForPromptReceipt_PreservedRolloutIgnoresSameCwdSibling(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	done := make(chan error, 1)
	go func() {
		done <- WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 2*time.Second)
	}()

	// A couple of polls establish the single-candidate identity, then a second
	// config agent's rollout lands in the same directory — the state that made
	// every remaining check ambiguous before identity was preserved.
	time.Sleep(200 * time.Millisecond)
	sibling := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)
	appendCodexRolloutEvent(t, sibling, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "a sibling config agent's briefing"},
	})
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("a same-cwd sibling aborted the receipt wait: %v", err)
	default:
	}

	appendCodexRolloutEvent(t, mine, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the pinned rollout's receipt was not observed")
	}
}

// A sibling can BEAT the launched rollout to the store: two config agents
// share AF home, so a poll can see the sibling as the lone same-directory
// candidate before the launched process's own file exists. Attribution must
// wait for the prompt match, never for first-seen — pinning that lone sibling
// would scan the wrong transcript forever and report never-arrived while the
// real receipt sits in the file that arrived second (#5471 review).
func TestWaitForPromptReceipt_SiblingRolloutFirstDoesNotStealReceipt(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	sibling := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	appendCodexRolloutEvent(t, sibling, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "a sibling config agent's briefing"},
	})

	done := make(chan error, 1)
	go func() {
		done <- WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 2*time.Second)
	}()

	// Polls observe the sibling alone first; the launched rollout lands after.
	time.Sleep(200 * time.Millisecond)
	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)
	appendCodexRolloutEvent(t, mine, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a first-seen sibling stole the receipt identity from the later real rollout")
	}
}

// A `codex resume` launch appends to a rollout that predates the snapshot —
// the before-set already holds it, so the new-file scan can never see it. The
// tracked path is the only thing pointing the receipt check at the file the
// resumed session will actually write (#5471 review).
func TestWaitForPromptReceipt_TrackedResumedRollout(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	resumed := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	require.True(t, snap.TrackCodexReceiptRollout(resumed))

	appendCodexRolloutEvent(t, resumed, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0),
		"the tracked resumed rollout is where the briefing lands")
}

// A resumed transcript can already hold the identical briefing from an
// earlier run: matching that stale turn would confirm a receipt this launch
// never earned. Only turns appended after tracking begins count (#5471
// review).
func TestWaitForPromptReceipt_ResumedRolloutIgnoresPreTrackTurns(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	resumed := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	appendCodexRolloutEvent(t, resumed, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	require.True(t, snap.TrackCodexReceiptRollout(resumed))

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"the same briefing recorded by an earlier run is not this launch's receipt")
}

// A tracked path the stat could not bound reports false: without a recorded
// size boundary there is no way to keep pre-launch turns from counting as
// this launch's receipt, so the resume target degrades to unidentified
// (#5471 review).
func TestTrackCodexReceiptRollout_UnstattablePathRefuses(t *testing.T) {
	snap := BeginConversationCaptureAtCodexHome(t.TempDir())
	require.False(t, snap.TrackCodexReceiptRollout(filepath.Join(t.TempDir(), "gone.jsonl")))
}

// CodexResumedRolloutPath maps a `codex resume` launch onto the rollout it
// will append to — the named conversation id, or the newest same-directory
// file for --last, which Codex scopes to the session cwd — and reports
// "resume, but target unknown" rather than guessing when the target cannot be
// identified (#5471 review).
func TestCodexResumedRolloutPath(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	// Three orderings disagree deliberately. Filename timestamps give the
	// creation order stale > foreign > older > recent; mtimes make stale
	// the freshest; the last recorded event gives foreign > recent > older
	// = stale. Codex 0.164 resolves --last by ThreadSortKey::UpdatedAt —
	// the recorded recency, immune to mtime bumps — so --last must pick
	// `recent` (created-oldest, updated-newest in cwd) and --all `foreign`.
	older := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	recent := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-34-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir)
	stale := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-39-059f386f-7206-7fc2-803b-f7045e07a246.jsonl", workDir)
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-37-039f386f-7206-7fc2-803b-f7045e07a244.jsonl", otherDir)
	for path, ts := range map[string]string{
		older:   "2026-07-06T10:17:35Z",
		recent:  "2026-07-06T10:17:38Z",
		stale:   "2026-07-06T10:17:35Z",
		foreign: "2026-07-06T10:17:39Z",
	} {
		appendCodexRolloutEvent(t, path, map[string]any{"timestamp": ts})
	}
	require.NoError(t, os.Chtimes(stale, time.Now(), time.Now()))

	for _, tc := range []struct {
		name       string
		program    string
		workingDir string
		wantPath   string
		wantResume bool
	}{
		{"not a codex command", "claude --resume 019f386f-7206-7fc2-803b-f7045e07a242", workDir, "", false},
		{"plain codex", "codex", workDir, "", false},
		{"explicit id", "codex resume 019f386f-7206-7fc2-803b-f7045e07a242", workDir, older, true},
		{"id ignores the cwd filter", "codex resume 039f386f-7206-7fc2-803b-f7045e07a244", workDir, foreign, true},
		{"id after global flags", "codex -c check_for_update_on_startup=false resume 019f386f-7206-7fc2-803b-f7045e07a242", workDir, older, true},
		{"latest picks most recently updated in cwd", "codex resume --last", workDir, recent, true},
		{"latest --all picks globally newest update", "codex resume --last --all", workDir, foreign, true},
		{"all before last still unscoped", "codex resume --all --last", workDir, foreign, true},
		{"latest through an env wrapper", "env FOO=1 codex resume --last", workDir, recent, true},
		{"latest without a cwd is unresolvable", "codex resume --last", "", "", true},
		{"bare resume opens the picker", "codex resume", workDir, "", true},
		{"unknown id", "codex resume 00000000-0000-0000-0000-000000000000", workDir, "", true},
		{"id beats --last", "codex resume --last 019f386f-7206-7fc2-803b-f7045e07a242", workDir, older, true},
		{"option value is not an id", "codex resume -m gpt-5 --last", workDir, recent, true},
		{"option value before an id", "codex resume -m gpt-5 019f386f-7206-7fc2-803b-f7045e07a242", workDir, older, true},
		{"attached option value", "codex resume --model=gpt-5 --last", workDir, recent, true},
		{"unmodelled flag is unprovable", "codex resume --include-non-interactive --last", workDir, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, resume := CodexResumedRolloutPath(codexHome, tc.program, tc.workingDir)
			require.Equal(t, tc.wantResume, resume)
			require.Equal(t, tc.wantPath, path)
		})
	}
}

// `codex resume --last` is scoped to the session's working directory: when
// the globally newest rollout belongs to another cwd it is NOT the resumed
// conversation, and tracking it would scan a stranger's transcript (#5471
// review).
func TestCodexResumedRolloutPath_LastIsWorkingDirScoped(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", otherDir)
	appendCodexRolloutEvent(t, mine, map[string]any{"timestamp": "2026-07-06T10:17:35Z"})
	appendCodexRolloutEvent(t, foreign, map[string]any{"timestamp": "2026-07-06T10:17:36Z"})
	stale := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mine, stale, stale))

	path, resume := CodexResumedRolloutPath(codexHome, "codex resume --last", workDir)
	require.True(t, resume)
	require.Equal(t, mine, path,
		"a newer rollout in another working directory is not what --last resumes")
}

// A store that cannot be enumerated is not an empty store: the late re-check's
// "never arrived" must only fire when the scan really looked. Traversal
// failures surface as their own indeterminate error, never as absence (#5471
// review).
func TestWaitForPromptReceipt_UninspectedStoreIsIndeterminate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()
	snap := BeginConversationCaptureAtCodexHome(codexHome)
	sessionsDir := filepath.Join(codexHome, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0755))
	require.NoError(t, os.Chmod(sessionsDir, 0))
	t.Cleanup(func() { _ = os.Chmod(sessionsDir, 0755) })

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPromptReceiptNotObserved,
		"a store the scan could not enter is not an observed absence")
	require.ErrorContains(t, err, "scan of the Codex rollout store failed")
}

// And while the window is still open the undecided rollout gets the same grace
// the receipt wait gives a slow one: it can settle into a real receipt.
func TestWaitForPromptReceipt_UncorrelatedSettlesIntoReceipt(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	const name = "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl"
	writeEmptyCodexRolloutFile(t, codexHome, name)

	done := make(chan error, 1)
	go func() {
		done <- WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 2*time.Second)
	}()

	time.Sleep(200 * time.Millisecond)
	path := writeCodexRolloutFileWithCwd(t, codexHome, name, workDir)
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a receipt whose rollout settled mid-wait was never observed")
	}
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

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
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
	return writeCodexRolloutFileWithSource(t, codexHome, name, cwd, "cli")
}

// writeCodexRolloutFileWithSource writes a session_meta whose source is the
// given kind — a string for plain variants, a map for data-bearing ones like
// {"subagent":"review"}, and nil to omit the key entirely (rollouts that
// predate the field).
func writeCodexRolloutFileWithSource(t *testing.T, codexHome, name, cwd string, source any) string {
	t.Helper()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	payload := map[string]any{"cwd": cwd}
	if source != nil {
		payload["source"] = source
	}
	data, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": payload,
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

// CodexRolloutPathByID resolves only an explicit `codex resume <SESSION_ID>`
// selector: filename holds the id verbatim, the working-directory filter does
// not apply to a named id, and an id that matches no rollout — or several —
// resolves to nothing rather than guessing (#5471 review, #5486 direction).
func TestCodexRolloutPathByID(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-37-039f386f-7206-7fc2-803b-f7045e07a244.jsonl", otherDir)
	// Two distinct filenames can carry the same conversation id only by
	// corruption or a race mid-rename; either way the id cannot pin one file.
	dupID := "059f386f-7206-7fc2-803b-f7045e07a246"
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-38-"+dupID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-39-"+dupID+".jsonl", workDir)

	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"explicit id", "019f386f-7206-7fc2-803b-f7045e07a242", mine},
		{"id ignores the cwd filter", "039f386f-7206-7fc2-803b-f7045e07a244", foreign},
		{"uppercase id still matches", "019F386F-7206-7FC2-803B-F7045E07A242", mine},
		{"unknown id resolves nothing", "00000000-0000-0000-0000-000000000000", ""},
		{"duplicated id is unpinnable", dupID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CodexRolloutPathByID(codexHome, tc.id))
		})
	}
}

// TrackCodexReceiptPool's scoped set is the snapshot's working directory PLUS
// every rollout whose session metadata cannot be classified — unclassifiable
// is unknown, never proven foreign (#5471 review, #5486). Only a rollout whose
// meta positively reports another cwd is left out.
func TestTrackCodexReceiptPool_ScopedKeepsUnknownRollouts(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	mine := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", otherDir)
	// No classifiable session_meta at all: predates the field, still being
	// written, or malformed — the pool must keep it either way.
	unknown := writeCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-37-039f386f-7206-7fc2-803b-f7045e07a244.jsonl")

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)

	tracked := make(map[string]bool, len(snap.receiptPaths))
	for _, tr := range snap.receiptPaths {
		tracked[tr.path] = true
	}
	require.True(t, tracked[mine], "same-directory rollout must be in the pool")
	require.True(t, tracked[unknown],
		"unclassifiable metadata is unknown, not foreign — the pool must keep it")
	require.False(t, tracked[foreign],
		"a rollout proven to another working directory is out of scope")
}

// The unscoped pool (--all, the picker, an unprovable selector or launch
// directory) is stat-only: membership never reads a byte of transcript, so a
// rollout whose contents cannot be read at all still gets bounded by its
// size (#5486 approval notes).
func TestTrackCodexReceiptPool_UnscopedIsStatOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()
	// An unreadable rollout: any membership rule that touched contents would
	// fail to classify it; the stat-only pool must bound it anyway.
	unreadable := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", t.TempDir())
	require.NoError(t, os.Chmod(unreadable, 0))
	info, err := os.Stat(unreadable)
	require.NoError(t, err)

	snap := BeginConversationCaptureAtCodexHome(codexHome)
	snap.TrackCodexReceiptPool(true)

	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, unreadable, snap.receiptPaths[0].path)
	require.Equal(t, info.Size(), snap.receiptPaths[0].offset,
		"the recorded boundary is the stat size — no contents were needed")
}

// A briefing appended to a pool rollout AFTER tracking confirms the receipt;
// the same bytes already present before tracking cannot — the offset boundary
// is what keeps an earlier run's identical turn from counting (#5471 review).
func TestTrackCodexReceiptPool_ConfirmsOnlyAppendedBytes(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	// The resumed conversation may already hold the identical briefing from an
	// earlier run — bytes present before the boundary must not confirm.
	stale := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	appendCodexRolloutEvent(t, stale, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	resumed := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"the briefing byte-identical to a pre-launch turn must not count as this launch's receipt")

	appendCodexRolloutEvent(t, resumed, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	// The first check recorded the pool file as quiet, so its next stat
	// lands on the slow cadence rather than the next poll tick — allow the
	// wait a little longer than that interval (#5486 review).
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing",
		2*codexReceiptQuietStatInterval+time.Second),
		"bytes appended past a pool offset are this launch's receipt")
}

// Two pool rollouts that both record the briefing after launch stay
// non-negative: the verdict is ambiguous, never "not observed" — for an
// unpinned resume a negative would be a lie about an unpinned file (#5486
// approval notes).
func TestWaitForPromptReceipt_UnpinnedPoolTwoMatchesStayNonNegative(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	first := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	second := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)

	for _, path := range []string{first, second} {
		appendCodexRolloutEvent(t, path, map[string]any{
			"type":    "event_msg",
			"payload": map[string]any{"type": "user_message", "message": "config briefing"},
		})
	}
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPromptReceiptAmbiguous,
		"two post-launch matches must surface as ambiguous")
	require.NotErrorIs(t, err, ErrPromptReceiptNotObserved,
		"two post-launch matches must never report not-observed")
}

// Codex serializes data-bearing SessionSource variants as objects — a
// subagent rollout's session_meta carries "source":{"subagent":"review"} —
// and decoding that into a string field must not lose the rollout's cwd or
// a same-cwd subagent stays uncorrelatable forever (#5486 review).
func TestCodexRolloutWorkingDir_ObjectSourceKeepsCwd(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	subagent := writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir,
		map[string]any{"subagent": "review"})

	cwd, ok := codexRolloutWorkingDir(subagent)
	require.True(t, ok, "an object-shaped source must not discard a valid cwd")
	require.Equal(t, workDir, cwd)
}

// A session_meta larger than the old 1 MiB read cap is a valid record: Codex
// embeds full base_instructions and tool payloads in it. Truncating the record
// left the launch permanently uncorrelatable; the streamed read must deliver
// the complete line so the cwd still classifies (#5486 review).
func TestCodexRolloutWorkingDir_FirstRecordLargerThanOneMiBStillParses(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-41-059f386f-7206-7fc2-803b-f7045e07a246.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	meta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": workDir, "base_instructions": strings.Repeat("x", 2<<20)},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(meta, '\n'), 0644))

	cwd, ok := codexRolloutWorkingDir(path)
	require.True(t, ok, "a complete >1 MiB session_meta record must still classify")
	require.Equal(t, workDir, cwd)
}

// The streamed read keeps a hard bound: a first record larger than the cap
// stays unclassifiable rather than growing memory without limit.
func TestCodexRolloutWorkingDir_FirstRecordBeyondReadCapStaysUnclassifiable(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	path := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-42-069f386f-7206-7fc2-803b-f7045e07a247.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	meta, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": workDir, "base_instructions": strings.Repeat("y", codexSessionMetaReadCap)},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(meta, '\n'), 0644))

	_, ok := codexRolloutWorkingDir(path)
	require.False(t, ok, "a first record beyond the read cap must remain unclassifiable")
}

// A tracked pool file whose size has not passed its boundary is never opened,
// and a quiet file is only re-statted on the slow cadence — poll cost must not
// scale as pool size x the 50ms tick (#5486 review). Once the file grows past
// the boundary it promotes to the fast path and its appended turn is found.
func TestCodexReceiptScan_QuietPoolFileStatsOnSlowCadence(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	path := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	info, err := os.Stat(path)
	require.NoError(t, err)
	offset := info.Size()

	state := newCodexReceiptState()
	matched, err := state.scanPrompt(path, "config briefing", offset)
	require.NoError(t, err)
	require.False(t, matched, "a file at its boundary holds no post-boundary bytes")

	// Bytes land within the quiet window: the throttled re-check must answer
	// from the last stat without noticing — that is the slow cadence working.
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	matched, err = state.scanPrompt(path, "config briefing", offset)
	require.NoError(t, err)
	require.False(t, matched, "a quiet file is not re-statted inside the slow cadence")

	// After the cadence elapses the stat promotes the file to the fast path
	// and the appended turn confirms.
	scan := state.scans[path]
	scan.checked = time.Now().Add(-2 * codexReceiptQuietStatInterval)
	state.scans[path] = scan
	matched, err = state.scanPrompt(path, "config briefing", offset)
	require.NoError(t, err)
	require.True(t, matched, "a grown file must be re-statted and read after the quiet cadence")
}

// An unpinned pool member that cannot be opened is unobservable, not fatal:
// the other pool members still get their chance to confirm, and a pool where
// every file fails still reports clean absence rather than an aborted check —
// the verdict layer maps unpinned absence to indeterminate (#5486 review).
func TestWaitForPromptReceipt_PoolFileReadErrorDoesNotAbort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()
	workDir := t.TempDir()
	dead := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	resumed := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", workDir)

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)

	// Grow the dead file past its boundary so the check actually tries to
	// open it, then make it unreadable.
	appendCodexRolloutEvent(t, dead, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	require.NoError(t, os.Chmod(dead, 0))
	t.Cleanup(func() { _ = os.Chmod(dead, 0644) })

	appendCodexRolloutEvent(t, resumed, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0),
		"an unreadable pool member must not deny the file that did record the briefing")
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

// TestWaitForPromptReceipt_TrailingWhitespacePromptMatchesStrippedTurn is the
// #5488 reproduction: configagent.BuildBriefing returns a prompt that ends with
// trailing whitespace ("\n\n"), but Codex 0.144.6 trims a submitted turn before
// recording it (chat_composer.rs prepare_submission_text_*: text.trim()), so the
// recorded user_message has its trailing whitespace removed. The receipt matcher
// must trim the prompt's trailing whitespace to match the stripped recorded turn
// — without that, every real Codex config-agent spawn logs "briefing receipt was
// not observed" even though the briefing landed.
func TestWaitForPromptReceipt_TrailingWhitespacePromptMatchesStrippedTurn(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	// The delivered prompt ends in the same trailing whitespace BuildBriefing
	// produces ("\n\n"). A mini-briefing stands in for the real one; it is the
	// trailing edge that matters.
	prompt := "You are the agent-factory configuration assistant.\n\n## The settings\n\nkeep going\n\n"
	// Codex records the submitted turn with leading AND trailing whitespace
	// stripped (text.trim()); the matcher's job is to confirm this recorded
	// text against the untrimmed prompt.
	recorded := strings.Trim(prompt, " \t\r\n")
	appendCodexRolloutEvent(t, path, map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":    "user_message",
			"message": recorded,
		},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, time.Second),
		"a prompt ending in trailing whitespace must match the Codex-trimmed recorded turn")
}

// TestWaitForPromptReceipt_CRLFAndTrailingWhitespaceNormalized confirms the
// matcher trims trailing whitespace AFTER normalizing CRLF/CR line endings, so a
// prompt delivered with Windows line endings and a trailing newline still
// matches a Codex-recorded turn stored with LF endings and no trailing whitespace.
func TestWaitForPromptReceipt_CRLFAndTrailingWhitespaceNormalized(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	// CRLF line endings plus a trailing CRLF, modeling a paste that crossed a
	// Windows-shaped boundary.
	prompt := "line one\r\nline two\r\n\r\n"
	recorded := "line one\nline two"
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": recorded},
	})

	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, time.Second))
}

// TestWaitForPromptReceipt_TrailingWhitespacePromptDoesNotMatchUnrelatedTurn
// guards the false-positive direction of the #5488 fix: trimming the prompt's
// trailing whitespace must not let it match a recorded turn that does not
// contain the briefing at all.
func TestWaitForPromptReceipt_TrailingWhitespacePromptDoesNotMatchUnrelatedTurn(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "an unrelated user turn"},
	})

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config-agent briefing\n\n", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"a trimmed trailing-whitespace prompt must not match an unrelated recorded turn")
}

// TestWaitForPromptReceipt_WhitespaceOnlyPromptIsUndecidable guards the
// degenerate needle: a prompt that is empty after trimming the whitespace Codex
// strips would make strings.Contains match every rollout, so the matcher must
// refuse it as undecidable up front rather than polling to a timeout or falsely
// confirming.
func TestWaitForPromptReceipt_WhitespaceOnlyPromptIsUndecidable(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	snap := BeginConversationCapture()
	// A rollout that DOES contain a user turn: without the undecidable guard
	// an empty needle would falsely confirm against it.
	path := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	appendCodexRolloutEvent(t, path, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "someone else's turn"},
	})

	for _, prompt := range []string{"", "   ", "\n\n", "\t\r\n ", "\r"} {
		err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, time.Second)
		require.ErrorIs(t, err, ErrPromptReceiptUndecidable,
			"a whitespace-only prompt (%q) must be refused as undecidable, not matched or timed out", prompt)
	}
}

// Capture keys on the directory the launch command actually runs the agent in,
// as af composed it — not the worktree (#5861). `env -C /elsewhere codex` writes
// a rollout whose metadata cwd is /elsewhere.
func TestLaunchCaptureWorkingDirFollowsEnvChdir(t *testing.T) {
	worktree := t.TempDir()
	elsewhere := t.TempDir()

	assert.Equal(t, pathutil.ResolveForCompare(worktree), pathutil.ResolveForCompare(LaunchCaptureWorkingDir("codex", worktree)),
		"a plain command runs in the worktree")
	assert.Equal(t, pathutil.ResolveForCompare(elsewhere),
		pathutil.ResolveForCompare(LaunchCaptureWorkingDir("env -C "+elsewhere+" codex", worktree)),
		"env -C moves the effective directory")
	assert.Equal(t, pathutil.ResolveForCompare(elsewhere),
		pathutil.ResolveForCompare(LaunchCaptureWorkingDir("env --chdir="+elsewhere+" codex", worktree)))
	assert.Empty(t, LaunchCaptureWorkingDir("cd /tmp && codex", worktree),
		"a chdir af does not model leaves correlation unscoped, never the worktree")
	assert.Empty(t, LaunchCaptureWorkingDir("env -C \"$X\" codex", worktree), "a dynamic chdir is unprovable")
}

func TestCaptureAgentConversation_CodexUnderEnvChdirCorrelatesByEffectiveDir(t *testing.T) {
	codexHome := t.TempDir()
	worktree := t.TempDir()
	elsewhere := t.TempDir()
	program := "env -C " + elsewhere + " codex"
	const rollout = "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl"

	// Anti-vacuous: keyed on the WORKTREE (the defect), the rollout the agent
	// wrote in its effective directory is never found.
	before := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, worktree)
	writeCodexRolloutFileWithCwd(t, codexHome, rollout, elsewhere)
	conv, err := CaptureAgentConversation(tmux.ProgramCodex, before, 0)
	require.NoError(t, err)
	require.False(t, conv.HasID(), "correlating against the worktree misses an env -C launch")

	// Keyed on the effective directory it is captured.
	codexHome2 := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome2, LaunchCaptureWorkingDir(program, worktree))
	writeCodexRolloutFileWithCwd(t, codexHome2, rollout, elsewhere)
	conv, err = CaptureAgentConversation(tmux.ProgramCodex, snap, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
}

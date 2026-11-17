package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

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

	received, undecided, _, err := codexPromptReceiptOnce(snap, prompt)
	require.NoError(t, err)
	require.False(t, received, "a still-unclassifiable rollout could settle into a second match")
	require.True(t, undecided)

	// Once it settles into another working directory the match is unique
	// and the receipt confirms.
	writeCodexRolloutFileWithCwd(t, codexHome, settling, t.TempDir())
	received, undecided, _, err = codexPromptReceiptOnce(snap, prompt)
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

// #7470: only a pool member positively classified as this launch's can
// confirm a receipt. A member kept unknown because its classification lookup
// did not answer or failed is not foreign — it stays — but its match is
// inconclusive, never a receipt. Membership is re-judged on every check:
// once the recorded cwd resolves again the member proves foreign, drops out,
// and its match is discounted for good. Here a sibling Codex's rollout in
// another directory records the briefing while its cwd cannot resolve: the
// wait stays undecidable, then once the mount answers the same check answers
// not-observed rather than confirming another directory's rollout (#7298,
// #7299, #7337's production shape).
func TestReceiptPoolMemberUnclassifiedByFailureNeverConfirms(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	other := t.TempDir()
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", other)

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	// The recorded cwd's resolve fails: the member is kept unknown, never
	// proven foreign.
	var failing atomic.Bool
	failing.Store(true)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if failing.Load() && (path == other || strings.HasPrefix(path, other+string(filepath.Separator))) {
			return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return filepath.EvalSymlinks(path)
	}))
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, poolMemberFailed, snap.receiptPaths[0].membership,
		"precondition: the cwd's failed resolve keeps the member unclassified")

	appendCodexRolloutEvent(t, foreign, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0)
	require.ErrorIs(t, err, ErrPromptReceiptUndecidable,
		"a match in a member af could not classify is inconclusive, never a confirmation: %v", err)

	// The mount answers again: the member re-judges foreign and drops out,
	// so the same briefing answers absence. Re-judgement is paced to the
	// quiet cadence (#7475 review R3), so this check ages the member's last
	// judgement to owe it a retry.
	failing.Store(false)
	snap.receiptPaths[0].memberChecked = time.Now().Add(-time.Hour)
	err = WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"re-judged foreign, the rollout's match is discounted for good: %v", err)
}

// A pool member proven this launch's — its recorded cwd resolved to the
// launch directory — confirms its match: positive classification is what the
// receipt waits on (#7470). The sibling's rollout provably another
// directory's never entered the pool.
func TestReceiptPoolMemberPositivelyClassifiedConfirms(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", t.TempDir())

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, own, snap.receiptPaths[0].path)
	require.Equal(t, poolMemberPositive, snap.receiptPaths[0].membership)

	appendCodexRolloutEvent(t, own, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0),
		"a lone match in a positively classified member confirms the receipt")
}

// The unscoped pool (--all, the picker, an unprovable selector) can never
// positively classify: the resume may legitimately land in any directory's
// rollout, so af does not predict the file. Its members stay unjudged and a
// lone match still confirms — there is no scope claim to be wrong against
// (#7470).
func TestReceiptPoolUnscopedLoneMatchStillConfirms(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	foreign := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", t.TempDir())

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(true)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, poolMemberUnjudged, snap.receiptPaths[0].membership)

	appendCodexRolloutEvent(t, foreign, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0),
		"an unscoped pool's lone match confirms by its contract")
}

// A member whose header can never be judged — readable, but not a usable
// session_meta — holds its match inconclusive forever rather than confirming
// (#7470). That is the visible shape of a Codex format change, so a wait
// ending undecidable on it warns once, naming the rollout and why.
func TestReceiptPoolMemberUnclassifiableStaysInconclusiveAndWarns(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	// A session_meta with no payload: readable whole, never judgable — it
	// records no cwd.
	unclassifiable := writeCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	warnings := &logtest.Buffer{}
	log.CaptureOutput(t, log.WarningLog, "", warnings)

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, poolMemberUnclassifiable, snap.receiptPaths[0].membership)
	require.NotEmpty(t, snap.receiptPaths[0].memberWhy,
		"precondition: an unclassifiable member keeps why it can never be judged")

	appendCodexRolloutEvent(t, unclassifiable, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0)
	require.ErrorIs(t, err, ErrPromptReceiptUndecidable,
		"the lone match in a never-classifiable member stays inconclusive: %v", err)
	require.Contains(t, warnings.String(), unclassifiable,
		"the warning names the rollout af could never judge")
	require.Contains(t, warnings.String(), "no absolute cwd",
		"and why it can never be judged: %s", warnings.String())
}

// One check spends at most one bound on re-judgement and stops at the first
// non-answer, as the scan does (#7475 review R1): two members whose recorded
// cwds stall past the probe bound cost one stall, not one each — and a
// cancelled ctx runs no lookup at all. Re-judgement runs once per check in
// the wait, so a verdict it reaches persists.
func TestReceiptPoolRepooledStopsAtFirstNonAnswer(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	other1, other2 := t.TempDir(), t.TempDir()
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", other1)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-029f386f-7206-7fc2-803b-f7045e07a243.jsonl", other2)

	clock := fsprobe.NewManualClock(time.Now())
	t.Cleanup(fsprobe.SetFlightClockForTest(clock))

	// The mount answers with a definitive error at track — each member's
	// resolve completes and keeps it unclassified — then wedges: every
	// re-judgement resolve advances past the bound on the manual clock and
	// blocks, a stall the bound must cut, never a scheduling accident.
	var stallCalls atomic.Int32
	var wedged atomic.Bool
	release := make(chan struct{})
	defer close(release)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == other1 || path == other2 ||
			strings.HasPrefix(path, other1+string(filepath.Separator)) ||
			strings.HasPrefix(path, other2+string(filepath.Separator)) {
			if wedged.Load() {
				stallCalls.Add(1)
				clock.Advance(3 * time.Second)
				<-release
			}
			return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return filepath.EvalSymlinks(path)
	}))
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 2,
		"precondition: both members' failed resolves keep them unclassified")
	for i := range snap.receiptPaths {
		require.Equal(t, poolMemberFailed, snap.receiptPaths[i].membership)
		// Re-judgement is paced to the quiet cadence (#7475 review R3): age
		// both judgements so this check owes each a retry.
		snap.receiptPaths[i].memberChecked = time.Now().Add(-time.Hour)
	}
	wedged.Store(true)

	snap = snap.repooled(context.Background())
	require.Equal(t, int32(1), stallCalls.Load(),
		"re-judgement stops at the first non-answer: the second member is never probed")
	require.Len(t, snap.receiptPaths, 2,
		"a member that never answered is kept unclassified, never dropped nor confirmed")
	for _, tracked := range snap.receiptPaths {
		require.Equal(t, poolMemberFailed, tracked.membership)
	}

	// A cancelled ctx ends the pass the same way: no lookup is even attempted.
	for i := range snap.receiptPaths {
		snap.receiptPaths[i].memberChecked = time.Now().Add(-time.Hour)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	snap = snap.repooled(cancelled)
	require.Equal(t, int32(1), stallCalls.Load(),
		"a cancelled ctx performs no re-judgement")
	require.Len(t, snap.receiptPaths, 2)
}

// A first record still mid-write — an empty file, or a line without its
// newline — is transient, never unclassifiable by contract (#7475 review R2):
// the member is re-judged, and when the record lands the verdict settles
// like a lookup that recovered. Here an empty rollout's session_meta lands
// with this launch's cwd, then the briefing: the match confirms.
func TestReceiptPoolMemberIncompleteHeaderIsRejudgedAndConfirms(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	incomplete := writeEmptyCodexRolloutFile(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")

	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, poolMemberFailed, snap.receiptPaths[0].membership,
		"a first record with no newline is transient, not unclassifiable by contract")

	// The session_meta lands with this launch's cwd, then the briefing.
	appendCodexRolloutEvent(t, incomplete, map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": workDir, "source": "cli"},
	})
	appendCodexRolloutEvent(t, incomplete, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	snap.receiptPaths[0].memberChecked = time.Now().Add(-time.Hour)
	require.NoError(t, WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0),
		"re-judged positive once the record completes, the member's match confirms")
}

// A member whose lookup answers with a definitive error is failure-class,
// re-judged no faster than the 1s quiet cadence pool files restat on (#7475
// review R3) — not twice per poll. And a wait that ends undecidable on that
// failure-class matcher names it, as the contract case does.
func TestReceiptPoolFailedMemberRejudgementIsPacedAndNamed(t *testing.T) {
	const prompt = "config briefing 7470"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	other := t.TempDir()
	member := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", other)
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	var resolveCalls atomic.Int32
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == other || strings.HasPrefix(path, other+string(filepath.Separator)) {
			resolveCalls.Add(1)
			return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return filepath.EvalSymlinks(path)
	}))
	snap.TrackCodexReceiptPool(false)
	require.Len(t, snap.receiptPaths, 1)
	require.Equal(t, poolMemberFailed, snap.receiptPaths[0].membership)
	base := resolveCalls.Load()

	// Pacing: judged a tick ago the member owes no retry; aged past the
	// quiet cadence it re-judges once, then is paced again.
	snap = snap.repooled(context.Background())
	require.Equal(t, base, resolveCalls.Load(), "a member judged a tick ago is not re-walked")
	snap.receiptPaths[0].memberChecked = time.Now().Add(-time.Hour)
	snap = snap.repooled(context.Background())
	require.Equal(t, base+1, resolveCalls.Load(), "the aged member re-judges once")
	snap = snap.repooled(context.Background())
	require.Equal(t, base+1, resolveCalls.Load(), "and is then paced again")

	// The wait ends undecidable on the failure-class matcher, and names it.
	appendCodexRolloutEvent(t, member, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": prompt},
	})
	warnings := &logtest.Buffer{}
	log.CaptureOutput(t, log.WarningLog, "", warnings)
	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, prompt, 0)
	require.ErrorIs(t, err, ErrPromptReceiptUndecidable,
		"the lone match in a lookup-failed member stays inconclusive: %v", err)
	require.Contains(t, warnings.String(), member,
		"the warning names the rollout af could never classify")
	require.Contains(t, warnings.String(), "launch-directory lookup",
		"and why it was never classified: %s", warnings.String())
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
	rolloutDir := codexRolloutDayDir(codexHome, time.Now())
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

// A sub-agent's rollout that quotes the prompt back is not the launched
// process's receipt: it can never be this session's top-level conversation, so
// its contents are not receipt evidence either (#6766).
func TestWaitForPromptReceipt_SubagentRolloutIsNotReceiptEvidence(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := BeginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	subagent := writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir,
		map[string]any{"subagent": "review"})
	appendCodexRolloutEvent(t, subagent, map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "config briefing"},
	})

	err := WaitForPromptReceipt(context.Background(), tmux.ProgramCodex, snap, "config briefing", 0)
	require.ErrorIs(t, err, ErrPromptReceiptNotObserved,
		"a sub-agent's rollout is not evidence the pane's conversation recorded the prompt")
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

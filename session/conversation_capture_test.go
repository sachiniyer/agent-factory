package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func writeCodexRolloutFile(t *testing.T, codexHome, name string) string {
	t.Helper()
	// Rollouts a capture must observe must sit in the day dir Codex would
	// mint them in now: the bounded scan prunes directories provably older
	// than the snapshot window (#6845).
	path := filepath.Join(codexRolloutDayDir(codexHome, time.Now()), name)
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
	path := filepath.Join(codexRolloutDayDir(codexHome, time.Now()), name)
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

// bindRuntimeHeldFiles returns snap with its runtime channel bound to a fake
// whose open-file set is the given map — the test-side analogue of
// Instance.BindConversationCaptureRuntime. The map is read on every
// attribution, possibly from a capture goroutine, so it must NOT be mutated
// after binding; tests that grant held files mid-poll use codexHeldFixture.
func bindRuntimeHeldFiles(snap ConversationCaptureSnapshot, held map[string]struct{}) ConversationCaptureSnapshot {
	return bindRuntimeHeldFixture(snap, &codexHeldFixture{m: held})
}

// codexHeldFixture is a mutation-safe held set: grant synchronizes with the
// descriptor snapshot each judgement takes, so a poll in a capture goroutine
// and a test writing held files never share a map.
type codexHeldFixture struct {
	mu sync.Mutex
	m  map[string]struct{}
	// onSnapshot, when set, runs inside each descriptor snapshot. A snapshot
	// only happens while a classifiable candidate is being judged, so this is
	// also the "an attribution judgement ran" signal a test can wait on. It
	// must not call back into the fixture — the mutex is held.
	onSnapshot func()
}

func (h *codexHeldFixture) grant(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[path] = struct{}{}
}

func (h *codexHeldFixture) snapshot() (map[string]struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.onSnapshot != nil {
		h.onSnapshot()
	}
	cp := make(map[string]struct{}, len(h.m))
	for path := range h.m {
		cp[path] = struct{}{}
	}
	return cp, nil
}

func bindRuntimeHeldFixture(snap ConversationCaptureSnapshot, fixture *codexHeldFixture) ConversationCaptureSnapshot {
	verdicts := make(map[string]codexRolloutVerdict)
	var mu sync.Mutex
	snap.runtime = &codexRuntimeAttribution{attribute: func(candidates []string) ([]string, bool) {
		return judgeCodexRollouts(fixture.snapshot, verdicts, &mu, candidates)
	}}
	return snap
}

func TestCaptureAgentConversation_CodexRolloutFile(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-33-aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa.jsonl")

	snap := BeginConversationCapture()
	minted := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{minted: {}})

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

	first := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl")
	second := writeCodexRolloutFile(t, codexHome, "rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl")
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{first: {}, second: {}})

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

	mintedA := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDirA)
	mintedB := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDirB)
	snapA = bindRuntimeHeldFiles(snapA, map[string]struct{}{mintedA: {}})
	snapB = bindRuntimeHeldFiles(snapB, map[string]struct{}{mintedB: {}})

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

	first := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	second := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{first: {}, second: {}})

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.ErrorContains(t, err, "2 new rollout files are held open by the session's runtime")
	require.False(t, conv.HasID(), "same-cwd rollouts held by one runtime cannot be distinguished safely")
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

	mintedA := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDirA)
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{mintedA: {}})
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

// A sub-agent's rollout shares its parent's working directory but can never be
// the session's own top-level conversation (#6766). When the only new rollout
// is a sub-agent's, capture must keep waiting for the pane's own file rather
// than record the sub-agent's id — an id respawn would then resume.
func TestCaptureAgentConversation_CodexSubagentRolloutIsNeverTheConversation(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir,
		map[string]any{"subagent": "review"})

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
	require.NoError(t, err)
	require.False(t, conv.HasID(), "a sub-agent's rollout can never be this session's conversation")
}

// The session's own rollout and a sub-agent's arriving together used to refuse
// capture as ambiguous — two "new" files in the launch directory. A sub-agent
// is positively identified by its session_meta source, so capture follows the
// one file that can be the top-level conversation (#6766).
func TestCaptureAgentConversation_CodexSubagentDoesNotForceAmbiguity(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	minted := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{minted: {}})
	writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir,
		map[string]any{"subagent": "review"})

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
}

// The carried half of #6766: the rollout a swap copied in sits in codexBefore,
// so the ONLY new file capture can see is the sub-agent's — which before the
// source check "won" and replaced the carried conversation af just resumed.
// The carried id must stand.
func TestCaptureAgentConversation_CodexSubagentDoesNotStealCarriedConversation(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	carried := AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
		CaptureKind: ConversationCaptureCarried,
	}
	carriedPath := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-33-"+carried.ID+".jsonl")
	snap.expectCarriedCodexRollout(carried, carriedPath)

	writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7fc2-803b-f7045e07a245.jsonl", workDir,
		map[string]any{"subagent": "review"})

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, carried.ID, conv.ID,
		"the sub-agent's rollout must not replace the carried conversation id")
}

// TestCaptureAgentConversation_CodexSiblingRolloutDoesNotStealCarriedConversation
// is the top-level-sibling case the #6766 sub-agent fix left uncovered: the
// ONLY new file capture can see is an unrelated top-level rollout (source:
// "cli") in the same working directory — which before this fix "won" and
// replaced the carried conversation af just resumed. The carried id must
// stand: appearance alone is not positive evidence the replacement forked.
func TestCaptureAgentConversation_CodexSiblingRolloutDoesNotStealCarriedConversation(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	carried := AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
		CaptureKind: ConversationCaptureCarried,
	}
	carriedPath := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-33-"+carried.ID+".jsonl")
	snap.expectCarriedCodexRollout(carried, carriedPath)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7c20-803b-f7045e07a245.jsonl", workDir)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, carried.ID, conv.ID,
		"a sibling top-level rollout must not replace the carried conversation id")
	require.Equal(t, ConversationCaptureCarried, conv.CaptureKind,
		"the carried id is re-stamped as a carried observation, not a rollout capture")
}

// TestCaptureAgentConversation_CodexSiblingRolloutUnscopedDoesNotStealCarriedConversation
// is the unbound case: when the launch working directory is unprovable, every
// new top-level rollout anywhere in the store is a candidate, so a lone
// sibling anywhere can steal the identity. The carried id must stand.
func TestCaptureAgentConversation_CodexSiblingRolloutUnscopedDoesNotStealCarriedConversation(t *testing.T) {
	codexHome := t.TempDir()
	snap := BeginConversationCaptureAtCodexHome(codexHome)

	carried := AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
		CaptureKind: ConversationCaptureCarried,
	}
	carriedPath := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-33-"+carried.ID+".jsonl")
	snap.expectCarriedCodexRollout(carried, carriedPath)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-049f386f-7206-7c20-803b-f7045e07a245.jsonl", t.TempDir())

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, carried.ID, conv.ID,
		"a sibling rollout in an unscoped capture must not replace the carried conversation id")
}

// TestCaptureAgentConversation_CodexAmbiguousSiblingsDoNotStealCarriedConversation
// confirms that two same-cwd sibling rollouts — which fresh capture refuses
// as ambiguous — do not steal the carried id either: carried capture fail-closes
// to the carried id rather than guess among the new rollouts.
func TestCaptureAgentConversation_CodexAmbiguousSiblingsDoNotStealCarriedConversation(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	carried := AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
		CaptureKind: ConversationCaptureCarried,
	}
	carriedPath := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-33-"+carried.ID+".jsonl")
	snap.expectCarriedCodexRollout(carried, carriedPath)

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-35-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-36-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, carried.ID, conv.ID,
		"ambiguous sibling rollouts must not replace the carried conversation id")
}

// TestCaptureAgentConversation_CodexSiblingRolloutSettlesIntoOtherDirDoesNotStealCarried
// confirms that a sibling whose session_meta is initially unclassifiable — the
// undecided state capture waits out — does not steal the carried id once it
// settles into a different working directory or even the same one.
func TestCaptureAgentConversation_CodexSiblingRolloutSettlesIntoOtherDirDoesNotStealCarried(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	carried := AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c4d",
		CaptureKind: ConversationCaptureCarried,
	}
	carriedPath := filepath.Join(codexHome, "sessions", "2026", "07", "06",
		"rollout-2026-07-06T10-17-33-"+carried.ID+".jsonl")
	snap.expectCarriedCodexRollout(carried, carriedPath)

	// A sibling whose header is initially unclassifiable — the undecided
	// state capture waits out before fail-closing.
	const settling = "rollout-2026-07-06T10-17-40-049f386f-7206-7c20-803b-f7045e07a245.jsonl"
	writeEmptyCodexRolloutFile(t, codexHome, settling)

	done := make(chan captureResult, 1)
	go func() {
		conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
		done <- captureResult{conv, err}
	}()

	// Let the poll observe the undecided file, then settle it into the same
	// working directory — the lone same-cwd top-level sibling.
	time.Sleep(300 * time.Millisecond)
	writeCodexRolloutFileWithCwd(t, codexHome, settling, workDir)

	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, carried.ID, r.conv.ID,
			"a sibling that settled into the same cwd must not steal the carried id")
	case <-time.After(5 * time.Second):
		t.Fatal("carried capture did not resolve after the sibling settled")
	}
}

func writeEmptyCodexRolloutFile(t *testing.T, codexHome, name string) string {
	t.Helper()
	// Same directory as writeCodexRolloutFile so a later write to the same name
	// settles THIS file rather than creating a sibling.
	path := filepath.Join(codexRolloutDayDir(codexHome, time.Now()), name)
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
	held := &codexHeldFixture{m: make(map[string]struct{})}
	snap = bindRuntimeHeldFixture(snap, held)
	minted := writeEmptyCodexRolloutFile(t, codexHome, name)

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
	held.grant(minted)
	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, "019ffa56-4ff5-7f81-b31f-36d3fc702b3e", r.conv.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not resolve after the rollout header settled")
	}
}

// TestCaptureAgentConversation_CodexHoldVisibleAfterHeaderStillAdopts is the
// #6938 race made deterministic: the rollout's session_meta becomes
// classifiable while the runtime's hold is not yet visible, and a capture
// poll judges it inside that gap. A "not this session's" verdict rendered
// before the hold exists must not be final — once the hold is visible the
// rollout is positively this session's, and the capture must still return
// its id. The descriptor walk only runs while a classifiable candidate
// exists, so observing a judgement IS the poll between the header write and
// the grant; no timing luck is involved.
func TestCaptureAgentConversation_CodexHoldVisibleAfterHeaderStillAdopts(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	const name = "rollout-2026-08-13T01-56-28-019ffa56-4ff5-7f81-b31f-36d3fc702b3e.jsonl"
	held := &codexHeldFixture{m: make(map[string]struct{})}
	judged := make(chan struct{}, 1)
	held.onSnapshot = func() {
		select {
		case judged <- struct{}{}:
		default: // a full channel would wedge snapshot under its own mutex
		}
	}
	snap = bindRuntimeHeldFixture(snap, held)
	minted := writeEmptyCodexRolloutFile(t, codexHome, name)

	done := make(chan captureResult, 1)
	go func() {
		conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
		done <- captureResult{conv, err}
	}()

	// Let the base deadline pass with the header still unwritten — capture is
	// inside its settle window, the same state the #6938 flake raced in.
	time.Sleep(800 * time.Millisecond)
	writeCodexRolloutFileWithCwd(t, codexHome, name, workDir)

	// A judgement ran while the runtime held nothing: the poll between the
	// header write and the grant, forced rather than hoped for.
	select {
	case <-judged:
	case <-time.After(5 * time.Second):
		t.Fatal("no attribution judgement ran after the header settled")
	}
	held.grant(minted)

	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Equal(t, "019ffa56-4ff5-7f81-b31f-36d3fc702b3e", r.conv.ID,
			"a hold that becomes visible only after a foreign verdict must still attribute")
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not resolve after the runtime's hold became visible")
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
	held := &codexHeldFixture{m: make(map[string]struct{})}
	snap = bindRuntimeHeldFixture(snap, held)
	writeEmptyCodexRolloutFile(t, codexHome, first)

	done := make(chan captureResult, 1)
	go func() {
		conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
		done <- captureResult{conv, err}
	}()

	time.Sleep(400 * time.Millisecond)
	// A same-cwd sibling settles first; the poll must stay undecided while the
	// first header is still unclassifiable rather than capture the sibling.
	second := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-08-13T01-56-29-019ffa5a-ddb3-7941-8cad-7c688ceb97d4.jsonl", workDir)
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("capture must not decide while a candidate header is still settling: conv=%+v err=%v", r.conv, r.err)
	default:
	}
	// Both settled rollouts held by the same runtime: ambiguity, as inside the
	// base window.
	writeCodexRolloutFileWithCwd(t, codexHome, first, workDir)
	held.grant(filepath.Join(codexRolloutDayDir(codexHome, time.Now()), first))
	held.grant(second)

	select {
	case r := <-done:
		require.ErrorContains(t, r.err, "held open by the session's runtime")
		require.False(t, r.conv.HasID(), "same-cwd rollouts held by one runtime cannot be distinguished safely")
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
	minted := writeCodexRolloutFileWithCwd(t, codexHome2, rollout, elsewhere)
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{minted: {}})
	conv, err = CaptureAgentConversation(tmux.ProgramCodex, snap, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
}

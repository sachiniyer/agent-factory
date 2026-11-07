package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
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

// TestCaptureAgentConversation_CodexSiblingHeldByForeignProcessIsNotAdopted is
// the production shape of #6847: the snapshot IS bound to a runtime — the
// live, unprompted session — and that runtime provably holds nothing. The
// lone same-cwd rollout is provably another process's and the session stays
// unrecorded instead of being handed a foreign thread.
func TestCaptureAgentConversation_CodexSiblingHeldByForeignProcessIsNotAdopted(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	// The session's own runtime holds nothing yet — it waits unprompted.
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{})

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 200*time.Millisecond)
	require.NoError(t, err)
	require.False(t, conv.HasID(),
		"a rollout provably held by a different process is foreign evidence, not this session's conversation")
}

// TestCaptureAgentConversation_CodexOwnRolloutIsAdoptedAmongSiblings proves the
// fix's other half: attribution does not merely refuse, it RESOLVES. Two
// same-cwd rollouts that used to be unanswerably ambiguous are told apart by
// which one the runtime holds open — the session's own.
func TestCaptureAgentConversation_CodexOwnRolloutIsAdoptedAmongSiblings(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-41-019ffa5a-ddb3-7941-8cad-7c688ceb97d4.jsonl", workDir)
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{own: {}})

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, time.Second)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID,
		"the rollout the runtime holds open is its own conversation, siblings notwithstanding")
}

// TestCaptureAgentConversation_CodexAttributionWalkErrorIsInconclusive: a
// process set that cannot be enumerated is evidence that could not be read —
// undecided, not foreign — so capture keeps settling within its bound rather
// than adopting, erroring permanently, or pretending absence.
func TestCaptureAgentConversation_CodexAttributionWalkErrorIsInconclusive(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.runtime = &codexRuntimeAttribution{
		held: func() (map[string]struct{}, error) { return nil, errors.New("walk failed") },
	}

	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	conv, retry, err := captureCodexConversationOnce(snap)
	require.ErrorIs(t, err, errCodexCaptureUndecided)
	require.True(t, retry, "an unreadable runtime is a not-yet-answer, so it retries")
	require.False(t, conv.HasID())
}

// TestCaptureAgentConversation_CodexAttributionVerdictsAreMemoized: a rollout's
// owner never changes after mint, so a candidate judged once must not re-walk
// the process table on every poll — the armed loop re-asks indefinitely.
func TestCaptureAgentConversation_CodexAttributionVerdictsAreMemoized(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	calls := 0
	snap.runtime = &codexRuntimeAttribution{
		held: func() (map[string]struct{}, error) {
			calls++
			return map[string]struct{}{}, nil
		},
	}
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	for i := 0; i < 3; i++ {
		conv, retry, err := captureCodexConversationOnce(snap)
		require.NoError(t, err)
		require.True(t, retry)
		require.False(t, conv.HasID())
	}
	require.Equal(t, 1, calls, "a foreign verdict is judged once, not re-walked on every poll")
}

// TestCodexRuntimeHeldPaths exercises the real channel end to end: a process
// the instance's runtime evidence names holds a file open, and the walk finds
// it in that process's open descriptor set.
func TestCodexRuntimeHeldPaths(t *testing.T) {
	heldFile := filepath.Join(t.TempDir(), "held.jsonl")
	require.NoError(t, os.WriteFile(heldFile, []byte("x\n"), 0o644))

	// The holder keeps the file open in a real process the process table can
	// see. A grandchild exercises the same process-tree membership the pane
	// root's descendants get.
	holder := exec.Command("tail", "-f", heldFile)
	require.NoError(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill() })

	var root proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(holder.Process.Pid)
		if err == nil {
			root = p
		}
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the holder must be visible in the process table")

	inst := &Instance{}
	inst.runtimePID, inst.runtimeStartID = root.PID, root.StartID

	var held map[string]struct{}
	require.Eventually(t, func() bool {
		paths, err := inst.codexRuntimeHeldPaths()
		if err != nil {
			return false
		}
		held = paths
		return true
	}, 5*time.Second, 20*time.Millisecond, "the pane-member walk must resolve")
	_, ok := held[heldFile]
	require.True(t, ok, "a file open in the runtime's own process set must be found")
}

package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7101 suite. Capture used to identify a Codex conversation only as a
// rollout NEW since a baseline, so a baseline taken after the runtime's
// rollout already existed — a daemon restart, a deferred arm that claimed a
// swap's replacement generation after its mission minted the file, a pane
// that `/resume`d an older thread — could never record it, and every later
// attempt read as ordinary, silent absence. The runtime holding the file open
// (#6847's evidence) needs no baseline at all.

const (
	heldTestOwnID     = "01a1cc97-42b0-7022-9c38-f9b73b6425d3"
	heldTestSiblingID = "01a1cc95-f883-7252-b370-6e9ab3c41145"
)

// heldTestInstance is a pane-less instance whose runtime holds exactly the
// files in held — the instance-side binding production capture uses, with the
// descriptor walk handed a fixed answer.
func heldTestInstance(t *testing.T, held ...string) *Instance {
	t.Helper()
	inst := &Instance{}
	var mu sync.Mutex
	set := make(map[string]struct{}, len(held))
	for _, path := range held {
		set[path] = struct{}{}
	}
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		mu.Lock()
		defer mu.Unlock()
		cp := make(map[string]struct{}, len(set))
		for path := range set {
			cp[path] = struct{}{}
		}
		return cp, nil
	})
	return inst
}

// TestCaptureAgentConversation_CodexRecordsTheHeldRolloutItsBaselineAlreadyHas
// is the restart / mid-swap shape: the replacement's mission minted its
// rollout BEFORE the capture that is now armed took its baseline. The file is
// in the before-set, so the new-since-baseline diff can never see it — but
// the runtime holds it open, which is positive evidence on its own.
func TestCaptureAgentConversation_CodexRecordsTheHeldRolloutItsBaselineAlreadyHas(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	// The late baseline: the daemon restarted, or the poll's deferred arm
	// claimed the replacement's generation after the mission landed.
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	inst := heldTestInstance(t, own)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap), 100*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID,
		"the rollout the runtime holds open is its conversation even when a late baseline already lists it")
	require.Equal(t, tmux.ProgramCodex, conv.Agent)
	require.Equal(t, ConversationCaptureCodexRollout, conv.CaptureKind)
}

// TestCaptureAgentConversation_CodexRecordsAHeldResumedThreadOutsideTheScanWindow
// is the `/resume` shape: the pane resumed a thread minted weeks ago, whose
// day directory the bounded store scan never descends into (#6845). The
// held-open answer names the file directly, so no store walk is needed.
func TestCaptureAgentConversation_CodexRecordsAHeldResumedThreadOutsideTheScanWindow(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	resumed := writeCodexRolloutAt(t, codexHome, workDir, old, codexRolloutName(old, heldTestOwnID))
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	inst := heldTestInstance(t, resumed)

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap), 100*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID,
		"a resumed thread the runtime holds must be recorded wherever it sits in the store")
}

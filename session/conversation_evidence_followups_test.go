package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7131 suite: #7127's independent review, R1, R3 and R4 at the session
// layer.

// TestKeptCaptureEvidence_AReattachSettleReprovesAVerifiedIdentity is R1. A
// kept identity is trusted no longer than launch evidence would be: launch
// evidence is re-proven by every reattach settle, so a verified kept identity
// must be too. Otherwise a Lost-restore reattach over a session whose panes no
// longer root the agent — which survived outside them — leaves capture judging
// from the orphaned process for the rest of the daemon life.
func TestKeptCaptureEvidence_AReattachSettleReprovesAVerifiedIdentity(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", workDir)
	agent := startRolloutHolder(t, own)
	other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
	inst, panes := reattachInstance(t, agent, other)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	inst.settleReattachVerification(inst.Tabs[0].tmux) // unanswerable: the identity is kept
	panes.set(paneRootedAt(agent))
	require.NoError(t, inst.CheckConversationCaptureEvidence())
	held, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, held.ID, "fixture: the kept identity verified")

	// Later in the same daemon life the session's panes are rooted only at
	// another process, while the agent survives outside them; a Lost-restore
	// reattaches the session.
	panes.set(paneRootedAt(other))
	verified, claimRetired := inst.settleReattachVerification(inst.Tabs[0].tmux)
	require.False(t, verified)
	require.False(t, claimRetired, "no launch claim to retire: the verdicts keep their meaning")

	_, err = HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.ErrorIs(t, err, ErrNoConversationCaptureEvidence,
		"a reattach must re-prove the kept identity before capture judges from it again")
	require.ErrorIs(t, inst.CheckConversationCaptureEvidence(), ErrNoConversationCaptureEvidence)
	require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence,
		"the re-scan answered that no pane is rooted at it, so it is dropped")
	require.Zero(t, panes.asked.Load(), "neither the settle nor the re-scan asks which pane is active")
}

// TestRetakeSessionCodexCaptureBaseline_ReturnsWhyInsteadOfLogging is R3's
// second half. The capture loop retakes a failed baseline on every re-ask; when
// the retired row's project config cannot be read, SessionCodexCaptureBaseline
// logged the same warning on every call, outside the loop's per-error dedup.
// The retake must hand the reason back to its caller, which warns once.
func TestRetakeSessionCodexCaptureBaseline_ReturnsWhyInsteadOfLogging(t *testing.T) {
	noRetryPause(t)
	inst := &Instance{Title: "retired", Program: tmux.ProgramCodex, Path: t.TempDir()}
	rc := NewRestoreContext()
	rc.repoFromPath = func(string) (*config.RepoContext, error) {
		return nil, fmt.Errorf("failed to get git repo root: %w", errors.Join(config.ErrRepoProbeUnanswered, errors.New("fork/exec git: resource temporarily unavailable")))
	}
	rc.attempts = 1
	inst.restoreCtx.Store(rc)
	warnings := &logtest.Buffer{}
	log.CaptureOutput(t, log.WarningLog, "", warnings)

	snap, err := inst.RetakeSessionCodexCaptureBaseline()
	require.True(t, IsProjectConfigUnknown(err), "the retake reports why it has no baseline: %v", err)
	require.False(t, snap.CapturesCodex())
	require.NotContains(t, warnings.String(), "no conversation capture baseline",
		"the reason goes to the caller's deduplicated warning, not straight to the log")
}

// TestKeepCaptureEvidenceForTest_LeavesTheRetireShape is R4's anchor: the
// daemon's end-to-end property-2 tests seed the kept identity through this
// seam, so it must leave exactly what an unanswerable reattach probe's retire
// leaves — launch evidence and command cleared, the identity kept, the runtime
// evidence generation bumped — or those tests pin a population that does not
// exist (a retired row re-resolves its baseline from config, not RuntimeProgram).
func TestKeepCaptureEvidenceForTest_LeavesTheRetireShape(t *testing.T) {
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	settled, _ := reattachInstance(t, agent, agent)
	settledGen := settled.runtimeEvidenceGeneration.Load()
	settled.settleReattachVerification(settled.Tabs[0].tmux)

	seeded, _ := reattachInstance(t, agent, agent)
	seededGen := seeded.runtimeEvidenceGeneration.Load()
	seeded.KeepCaptureEvidenceForTest(agent)

	require.Equal(t, settled.runtimeProgram, seeded.runtimeProgram)
	require.Empty(t, seeded.RuntimeProgram(), "a retired row has no runtime command")
	require.Equal(t, settled.runtimePID, seeded.runtimePID)
	require.Equal(t, settled.runtimeStartID, seeded.runtimeStartID)
	require.Equal(t, settled.captureEvidence, seeded.captureEvidence)
	require.Equal(t, settled.runtimeEvidenceGeneration.Load()-settledGen, seeded.runtimeEvidenceGeneration.Load()-seededGen,
		"the seam bumps the runtime-evidence generation exactly as the retire does")
}

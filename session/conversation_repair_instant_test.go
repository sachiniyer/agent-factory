package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7147: when a standing operator repair's runtime counts as replaced. Any
// thread written after that instant is a move; writes before it are the old
// runtime's, judged by the repair's stamp (#7132). The instant must be as
// early as af can defend, and one af cannot place in the past bounds nothing.

// instantFixture is a store with R, the operator's choice, and H, the thread
// the pane held at the repair, stored before it. It returns H's path.
func instantFixture(t *testing.T, now, repairedAt time.Time) (workDir, held string) {
	t.Helper()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir = t.TempDir()
	writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	held = writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, repairedAt.Add(-time.Hour), repairedAt.Add(-time.Hour), repairedAt.Add(-time.Minute))
	return workDir, held
}

func writtenAt(t *testing.T, path string, at time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, at, at))
}

// G-1: a replacement instant dated after now — a launch recorded while the
// clock ran fast, before NTP stepped it back — is one af cannot place, so it
// bounds nothing: every write since the repair is a move, failing closed as a
// CapturedAt after now does. Otherwise the new pane's work in H, written
// "before" that future instant, would be exempt by the old stamp.
func TestOperatorRepairReplacementInstantAfterNowFailsClosed(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	observedH := operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID)
	for _, c := range []struct {
		name       string
		replacedAt time.Time
		resumes    bool
	}{
		{"recorded an hour ahead of the clock", now.Add(time.Hour), false},
		{"control: recorded after H's write, in the past", now.Add(-5 * time.Minute), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			workDir, held := instantFixture(t, now, repairedAt)
			writtenAt(t, held, now.Add(-10*time.Minute))
			restore := staleResumeInstance(t, workDir, observedH.WithRepairRuntimeReplaced(c.replacedAt))
			program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
			if c.resumes {
				require.True(t, ok, "refused: %v", decline)
				require.Contains(t, program, "resume "+heldTestOwnID)
				return
			}
			require.False(t, ok, "resumed %q although af cannot say H was written before the replacement", program)
			require.Equal(t, staleCarryReason, decline.reason)
		})
	}
	t.Run("the window reads a future instant as the repair's own date", func(t *testing.T) {
		w := operatorRepairWindowOf(observedH.WithRepairRuntimeReplaced(now.Add(time.Hour)), now)
		require.True(t, w.replacedAt.Equal(repairedAt))
		w = operatorRepairWindowOf(observedH.WithRepairRuntimeReplaced(now.Add(-time.Minute)), now)
		require.True(t, w.replacedAt.Equal(now.Add(-time.Minute).Round(0)))
	})
}

// G-2: a launch dates the replacement at the instant tmux took just before
// the spawn (TmuxSession.SpawnedAt), not when af finished recording the
// launch. Between the two — the
// ready poll and the tmux calls after it, seconds under load — the new pane is
// already interactive, and a thread it resumes and writes there is a move. The
// old runtime is dead before af spawns, so nothing it wrote is after that
// instant.
func TestOperatorRepairReplacementIsDatedAtTheSpawn(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	spawnedAt := now.Add(-time.Minute)
	for _, c := range []struct {
		name    string
		written time.Time
		resumes bool
	}{
		{"the new pane wrote H after the spawn, before af recorded the launch", now.Add(-30 * time.Second), false},
		{"control: the old pane wrote H before the spawn", now.Add(-2 * time.Minute), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			workDir, held := instantFixture(t, now, repairedAt)
			inst := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
			inst.setRuntimeLaunch(tmux.ProgramCodex, nil, spawnedAt)
			if !c.resumes {
				require.True(t, inst.AgentConversation().RepairRuntimeReplacedAt.Equal(spawnedAt.Round(0)),
					"the replacement must be dated at the spawn, not at %s", inst.AgentConversation().RepairRuntimeReplacedAt)
			}
			writtenAt(t, held, c.written)

			restore := staleResumeInstance(t, workDir, inst.AgentConversation())
			program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
			if c.resumes {
				require.True(t, ok, "refused: %v", decline)
				require.Contains(t, program, "resume "+heldTestOwnID)
				return
			}
			require.False(t, ok, "resumed %q over the thread the new pane resumed while af recorded its launch", program)
			// G-5: the reason names this case too.
			require.Contains(t, decline.reason, "or an older one was resumed")
		})
	}
	t.Run("a launch with no spawn instant, or one after now, is dated when it is recorded", func(t *testing.T) {
		before := time.Now()
		require.True(t, launchReplacementInstant(time.Time{}, before).Equal(before))
		require.True(t, launchReplacementInstant(before.Add(time.Hour), before).Equal(before),
			"a spawn instant after now would hand the new pane's first writes to the old stamp")
		require.True(t, launchReplacementInstant(spawnedAt, before).Equal(spawnedAt))
		// Codex review on #7150: two time.Now() readings compare by their
		// monotonic clock, which a wall-clock step does not move, so the
		// rollback check must compare — and the launch must persist — wall
		// readings.
		got := launchReplacementInstant(time.Now(), time.Now())
		require.Equal(t, got.Round(0), got, "the instant must be a wall-clock reading, not a monotonic one")
	})
}

// G-3: #7133's re-scan of a kept identity drops it on two answers — the
// process is gone or its pid was reused, or no pane of the session is rooted
// at it. Both say the runtime af launched is no longer the one in the pane,
// so a standing repair records the replacement as the reattach settle's
// answered mismatch does. Otherwise the process now in the pane can resume H
// and the next restore resumes R over it. An unanswerable scan keeps the
// identity and records nothing: the pane may still be the repair's runtime.
func TestOperatorRepairRecordsTheReplacementWhenAKeptIdentityIsDropped(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
	gone := exitedProcess(t)
	for _, c := range []struct {
		name    string
		kept    bool // the kept identity is the live agent rather than one that exited
		answer  func() ([]byte, error)
		resumes bool
	}{
		{"no pane is rooted at the kept identity", true, paneRootedAt(other), false},
		{"the kept identity's process is gone", false, paneRootedAt(other), false},
		{"control: the scan could not answer", true, paneProbeUnanswerable, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			workDir, held := instantFixture(t, now, repairedAt)
			keptProcess := gone
			if c.kept {
				keptProcess = agent
			}
			inst, panes := reattachInstance(t, keptProcess, other)
			inst.settleReattachVerification(inst.Tabs[0].tmux) // unanswerable: kept
			require.Equal(t, keptCaptureEvidence{pid: keptProcess.PID, startID: keptProcess.StartID}, inst.captureEvidence)
			inst.Tabs[0].Conversation = operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID)
			panes.set(c.answer)
			require.ErrorIs(t, inst.CheckConversationCaptureEvidence(), ErrNoConversationCaptureEvidence)
			if !c.resumes {
				// Nothing settled while the scan ran (#7145's straddle refusal
				// does not apply), so the identity is dropped: what is under
				// test is whether that drop records the replacement.
				require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence, "fixture: the kept identity must be dropped")
			}
			// From here on, the process in the pane works in H. A second past
			// the scan, so no clock or filesystem granularity can put the
			// write at or before it.
			writtenAt(t, held, time.Now().Add(time.Second))

			restore := staleResumeInstance(t, workDir, inst.AgentConversation())
			program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
			if c.resumes {
				require.True(t, ok, "refused: %v", decline)
				require.Contains(t, program, "resume "+heldTestOwnID)
				return
			}
			require.False(t, ok, "resumed %q over the thread a process af did not launch works in", program)
		})
	}
	// #7145: a scan that a reattach settled during proves nothing after the
	// settle, so it drops nothing — and so records nothing either.
	t.Run("control: a scan a reattach straddled drops and records nothing", func(t *testing.T) {
		workDir, held := instantFixture(t, now, repairedAt)
		inst, panes := reattachInstance(t, agent, other)
		inst.settleReattachVerification(inst.Tabs[0].tmux) // unanswerable: kept
		inst.Tabs[0].Conversation = operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID)
		entered, release := make(chan struct{}), make(chan struct{})
		panes.set(func() ([]byte, error) {
			close(entered)
			<-release
			return paneRootedAt(other)() // tmux's answer from before the reattach
		})
		done := make(chan error, 1)
		go func() { done <- inst.CheckConversationCaptureEvidence() }()
		<-entered
		panes.set(paneRootedAt(agent))
		inst.settleReattachVerification(inst.Tabs[0].tmux)
		close(release)
		require.ErrorIs(t, <-done, ErrNoConversationCaptureEvidence)
		require.Equal(t, keptCaptureEvidence{pid: agent.PID, startID: agent.StartID}, inst.captureEvidence,
			"fixture: the straddled scan drops nothing")
		writtenAt(t, held, time.Now().Add(time.Second))

		restore := staleResumeInstance(t, workDir, inst.AgentConversation())
		program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
		require.True(t, ok, "refused: %v", decline)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
}

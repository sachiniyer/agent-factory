package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7140 session suite: F1 from #7128's independent review, N1 and N3 from
// #7133's.

// TestUnreadableNewRolloutHeaderIsAnUnavailableChannel is F1. A new rollout
// whose header af cannot open or read says nothing about which thread the
// runtime is on, only that af could not look — the same class as a store scan
// that fails. Left untagged, a fresh account swap failed its committed
// handoff on it after the replacement launched (#7126's property). It keeps
// its undecided wrap, so the settle wait still gives it time to become
// readable, and the carried path still treats it as a not-yet-answer.
func TestUnreadableNewRolloutHeaderIsAnUnavailableChannel(t *testing.T) {
	newRollout := func(t *testing.T) (snap ConversationCaptureSnapshot, codexHome, workDir string) {
		codexHome, workDir = t.TempDir(), t.TempDir()
		snap = beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
		require.NoError(t, snap.CodexBaselineErr())
		return snap, codexHome, workDir
	}

	t.Run("a header af may not open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission bits do not constrain root")
		}
		snap, codexHome, workDir := newRollout(t)
		path := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-12T10-00-00-"+heldTestOwnID+".jsonl", workDir)
		require.NoError(t, os.Chmod(path, 0))

		_, retry, err := captureCodexConversationOnce(snap)
		require.True(t, retry, "it still gets the settle window to become readable")
		require.ErrorIs(t, err, errCodexCaptureUndecided)
		require.ErrorIs(t, err, ErrCodexCaptureChannelUnavailable,
			"an unreadable header is a channel af could not read, not an answer")
		require.Contains(t, err.Error(), "permission denied", "the error says why af could not read it")
	})

	t.Run("a symlinked rollout, which af refuses to follow", func(t *testing.T) {
		snap, codexHome, workDir := newRollout(t)
		target := writeCodexRolloutFileWithCwd(t, t.TempDir(), "rollout-2026-11-12T10-00-00-"+heldTestOwnID+".jsonl", workDir)
		link := filepath.Join(codexRolloutDayDir(codexHome, snap.startedAt), filepath.Base(target))
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
		require.NoError(t, os.Symlink(target, link))

		_, _, err := captureCodexConversationOnce(snap)
		require.ErrorIs(t, err, ErrCodexCaptureChannelUnavailable)
	})

	// The guard: a header Codex has not finished writing is readable, just not
	// classifiable yet. That stays an undecided answer, never a channel.
	t.Run("a header still being written", func(t *testing.T) {
		snap, codexHome, _ := newRollout(t)
		path := filepath.Join(codexRolloutDayDir(codexHome, snap.startedAt), "rollout-2026-11-12T10-00-00-"+heldTestOwnID+".jsonl")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"cwd":"/tm`), 0o600))

		_, retry, err := captureCodexConversationOnce(snap)
		require.True(t, retry)
		require.ErrorIs(t, err, errCodexCaptureUndecided)
		require.NotErrorIs(t, err, ErrCodexCaptureChannelUnavailable)
	})
}

// TestKeptCaptureEvidence_AScanThatStraddlesASettleProvesNothingAfterIt is N1.
// R1 made every reattach settle clear a verified kept identity, but a check
// whose pane scan began before the settle could still mark it verified after
// it, from the answer tmux gave before the reattach; every later check then
// trusted it with no scan for the rest of the daemon life. No judgement after
// a settle may rest on a scan that began before it.
func TestKeptCaptureEvidence_AScanThatStraddlesASettleProvesNothingAfterIt(t *testing.T) {
	straddle := func(t *testing.T, beforeSettle func() ([]byte, error)) (*Instance, *reattachPanes, error, keptCaptureEvidence) {
		codexHome, workDir := t.TempDir(), t.TempDir()
		agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, codexHome,
			"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", workDir))
		other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
			"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
		inst, panes := reattachInstance(t, agent, other)
		inst.settleReattachVerification(inst.Tabs[0].tmux) // unanswerable: kept, unverified
		kept := inst.captureEvidence
		require.NotZero(t, kept.pid, "fixture: the identity is kept")

		entered, release := make(chan struct{}), make(chan struct{})
		answer := beforeSettle
		if answer == nil {
			answer = paneRootedAt(agent)
		}
		panes.set(func() ([]byte, error) {
			close(entered)
			<-release
			return answer() // what tmux answered before the reattach
		})
		done := make(chan error, 1)
		go func() { done <- inst.CheckConversationCaptureEvidence() }()
		<-entered

		// While that scan is in flight the session reattaches; afterwards its
		// panes are rooted at the agent again.
		panes.set(paneRootedAt(agent))
		verified, claimRetired := inst.settleReattachVerification(inst.Tabs[0].tmux)
		require.False(t, verified)
		require.False(t, claimRetired, "no launch claim: the verdicts keep their meaning")
		close(release)
		return inst, panes, <-done, kept
	}

	t.Run("a positive answer from before the settle marks nothing", func(t *testing.T) {
		inst, panes, err, _ := straddle(t, nil)
		require.ErrorIs(t, err, ErrNoConversationCaptureEvidence,
			"the straddling check cannot vouch for the identity after the settle")
		inst.mu.RLock()
		verified := inst.captureEvidence.verified
		inst.mu.RUnlock()
		require.False(t, verified, "a scan that began before the settle must not mark the identity verified after it")

		listed := panes.listed.Load()
		require.NoError(t, inst.CheckConversationCaptureEvidence(), "the next check re-proves it")
		require.Greater(t, panes.listed.Load(), listed, "with a scan that began after the settle")
	})

	t.Run("a negative answer from before the settle drops nothing", func(t *testing.T) {
		inst, _, err, kept := straddle(t, func() ([]byte, error) { return paneRootedAt()() })
		require.ErrorIs(t, err, ErrNoConversationCaptureEvidence)
		inst.mu.RLock()
		pair := inst.captureEvidence
		inst.mu.RUnlock()
		require.Equal(t, kept.pid, pair.pid, "the identity the settle kept survives a stale answer")
		require.NoError(t, inst.CheckConversationCaptureEvidence(), "and the next check re-proves it")
	})
}

// TestRetakeSessionCodexCaptureBaseline_DoesNotLogAnInvalidProjectConfig is
// N3. The loop retakes a failed baseline on every re-ask; a project config the
// repository answered as invalid degrades to the global one — the launch-class
// rule — and that degrade logged on every retake, outside the loop's dedup.
// The arm already said it; the retake must not say it every ten seconds.
func TestRetakeSessionCodexCaptureBaseline_DoesNotLogAnInvalidProjectConfig(t *testing.T) {
	noRetryPause(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	inst := &Instance{Title: "retired", Program: tmux.ProgramCodex, Path: t.TempDir()}
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		return &config.RepoContext{Root: path, IdentityRoot: path, ID: "invalid-config"}, nil
	}
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		return nil, errors.New("invalid value for program_overrides")
	}
	rc.attempts = 1
	inst.restoreCtx.Store(rc)
	warnings := &logtest.Buffer{}
	log.CaptureOutput(t, log.WarningLog, "", warnings)

	for range 3 {
		_, err := inst.RetakeSessionCodexCaptureBaseline()
		require.NoError(t, err, "an answered-invalid config still degrades to the global one")
	}
	require.Zero(t, strings.Count(warnings.String(), "failed to resolve repo config"),
		"the retake path must not log the degrade on every call")
}

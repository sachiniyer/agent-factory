package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The kept-evidence suite (#7123 property 2). A reattach probe that could not
// answer — a tmux timeout under load — used to retire the launch evidence
// exactly like one that answered "no pane is rooted at that identity", and
// the retire was persisted, so capture failed closed for the rest of that
// runtime. "Could not answer" is no evidence of a replacement: capture, and
// only capture, keeps the identity and re-verifies it with the session-wide
// pane scan — never the active pane.

// reattachPanes is tab 0's tmux session as the probes see it: list-panes
// answers whatever the test sets, and the active-pane query, which no
// verification may make, is only counted.
type reattachPanes struct {
	mu     sync.Mutex
	answer func() ([]byte, error)
	listed atomic.Int32
	asked  atomic.Int32
}

func (p *reattachPanes) set(answer func() ([]byte, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = answer
}

func (p *reattachPanes) list() ([]byte, error) {
	p.listed.Add(1)
	p.mu.Lock()
	answer := p.answer
	p.mu.Unlock()
	return answer()
}

// paneRootedAt answers list-panes with one live pane per process.
func paneRootedAt(procs ...proctree.Process) func() ([]byte, error) {
	return func() ([]byte, error) {
		var b strings.Builder
		for _, p := range procs {
			fmt.Fprintf(&b, "%d|0|\n", p.PID)
		}
		return []byte(b.String()), nil
	}
}

// paneProbeUnanswerable is the probe under load: tmux did not answer.
func paneProbeUnanswerable() ([]byte, error) {
	return nil, errors.New("lost server")
}

// reattachInstance is a reattached Codex row whose launch evidence names
// launched, and whose ACTIVE pane — which no probe may consult — is active.
func reattachInstance(t *testing.T, launched, active proctree.Process) (*Instance, *reattachPanes) {
	t.Helper()
	panes := &reattachPanes{answer: paneProbeUnanswerable}
	executor := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			switch s := c.String(); {
			case strings.Contains(s, "list-panes"):
				return panes.list()
			case strings.Contains(s, "display-message"):
				panes.asked.Add(1)
				return []byte(fmt.Sprintf("%d\n", active.PID)), nil
			}
			return nil, errors.New("unexpected tmux command in fixture: " + c.String())
		},
	}
	inst := &Instance{Title: "reattach", Tabs: []*Tab{newAgentTab(
		tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_reattach", tmux.ProgramCodex, nil, executor))}}
	inst.runtimeProgram = tmux.ProgramCodex
	inst.runtimePID, inst.runtimeStartID = launched.PID, launched.StartID
	return inst, panes
}

// TestSettleReattachVerification_AnUnanswerableProbeKeepsTheIdentityForCapture
// is the split retire. Everything that reads the launch evidence today sees
// the same retire as before — the claim, the interruption verdict, the
// limit-wall and handoff pins — and capture alone keeps the pair.
func TestSettleReattachVerification_AnUnanswerableProbeKeepsTheIdentityForCapture(t *testing.T) {
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	inst, panes := reattachInstance(t, agent, agent)

	verified, claimRetired := inst.settleReattachVerification(inst.Tabs[0].tmux)
	require.False(t, verified, "an unanswerable probe never verifies")
	require.True(t, claimRetired, "the load path's claim keeps its meaning")
	require.Zero(t, inst.runtimePID, "the launch evidence every other consumer reads is retired as before")
	require.Zero(t, inst.runtimeStartID)
	require.Empty(t, inst.runtimeProgram)
	require.Equal(t, keptCaptureEvidence{pid: agent.PID, startID: agent.StartID}, inst.captureEvidence,
		"could not answer is not evidence of a replacement: capture keeps the identity to re-verify")
	require.EqualValues(t, 1, panes.listed.Load(), "one probe settles the verdict")
	require.Zero(t, panes.asked.Load(), "the probe never asks which pane is active")
}

// TestSettleReattachVerification_AnAnsweredMismatchRetiresTheIdentity is the
// other arm: tmux answered, and no pane of the session is rooted at the
// recorded process — real evidence of a replacement, so nothing is kept.
func TestSettleReattachVerification_AnAnsweredMismatchRetiresTheIdentity(t *testing.T) {
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
	for _, tc := range []struct {
		name   string
		answer func() ([]byte, error)
	}{
		{"another process roots every pane", paneRootedAt(other)},
		{"tmux reports no pane at all", func() ([]byte, error) { return nil, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, panes := reattachInstance(t, agent, other)
			panes.set(tc.answer)
			verified, claimRetired := inst.settleReattachVerification(inst.Tabs[0].tmux)
			require.False(t, verified)
			require.True(t, claimRetired)
			require.Zero(t, inst.runtimePID)
			require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence, "an answered mismatch keeps nothing")
		})
	}
}

// TestKeptCaptureEvidence_ReverifiedFromTheSessionWideScanFeedsCapture is
// property 2 end to end inside the session layer: after an unanswerable
// reattach probe, a later scan that finds the agent rooting one of the
// session's panes — not the active one, which is a user's split holding its
// own thread — restores the diff and the held query to the agent's thread,
// and re-enables nothing else.
func TestKeptCaptureEvidence_ReverifiedFromTheSessionWideScanFeedsCapture(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	time.Sleep(20 * time.Millisecond)
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", workDir)
	split := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", workDir)
	agent := startRolloutHolder(t, own)
	splitPane := startRolloutHolder(t, split)
	inst, panes := reattachInstance(t, agent, splitPane)

	inst.settleReattachVerification(inst.Tabs[0].tmux)
	generation := inst.runtimeEvidenceGeneration.Load()

	_, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.ErrorIs(t, err, ErrNoConversationCaptureEvidence,
		"until the kept identity is re-verified, capture fails closed and says why")

	panes.set(paneRootedAt(splitPane, agent))
	require.NoError(t, inst.CheckConversationCaptureEvidence(),
		"the session-wide scan finds the agent rooting one of the session's panes")

	conv, err := CaptureAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap), 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "the diff judges from the re-verified agent, not the active pane")
	held, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, held.ID, "the held query judges from the re-verified agent")

	require.Zero(t, panes.asked.Load(), "no verification or attribution asks which pane is active")
	require.Zero(t, inst.runtimePID, "capture-only: the launch evidence the limit wall and handoff read stays retired")
	require.Empty(t, inst.runtimeProgram)
	require.Equal(t, generation, inst.runtimeEvidenceGeneration.Load(),
		"re-verifying for capture is not a runtime-evidence transition")
}

// TestKeptCaptureEvidence_ReverificationOutcomes pins what each answer does to
// the kept identity: an answered "no" or a process that is gone drops it for
// good, while "could not answer" keeps it for the next attempt.
func TestKeptCaptureEvidence_ReverificationOutcomes(t *testing.T) {
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
	kept := func(t *testing.T, p proctree.Process) (*Instance, *reattachPanes) {
		inst, panes := reattachInstance(t, p, other)
		inst.settleReattachVerification(inst.Tabs[0].tmux)
		require.Equal(t, keptCaptureEvidence{pid: p.PID, startID: p.StartID}, inst.captureEvidence)
		panes.listed.Store(0)
		return inst, panes
	}

	t.Run("an answered mismatch drops it", func(t *testing.T) {
		inst, panes := kept(t, agent)
		panes.set(paneRootedAt(other))
		require.ErrorIs(t, inst.CheckConversationCaptureEvidence(), ErrNoConversationCaptureEvidence)
		require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence)
		require.Error(t, inst.CheckConversationCaptureEvidence())
		require.EqualValues(t, 1, panes.listed.Load(), "a dropped identity is never probed again")
	})
	t.Run("an unanswerable probe keeps it", func(t *testing.T) {
		inst, panes := kept(t, agent)
		require.ErrorIs(t, inst.CheckConversationCaptureEvidence(), ErrNoConversationCaptureEvidence)
		require.Equal(t, keptCaptureEvidence{pid: agent.PID, startID: agent.StartID}, inst.captureEvidence)
		panes.set(paneRootedAt(agent))
		require.NoError(t, inst.CheckConversationCaptureEvidence(), "the next attempt re-verifies it")
		require.EqualValues(t, 2, panes.listed.Load())
		require.NoError(t, inst.CheckConversationCaptureEvidence())
		require.EqualValues(t, 2, panes.listed.Load(), "a verified identity needs no further scan")
	})
	t.Run("a process that is gone drops it without asking tmux", func(t *testing.T) {
		inst, panes := kept(t, exitedProcess(t))
		panes.set(paneRootedAt(agent))
		require.ErrorIs(t, inst.CheckConversationCaptureEvidence(), ErrNoConversationCaptureEvidence)
		require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence)
		require.Zero(t, panes.listed.Load())
	})
	t.Run("a launch boundary clears it", func(t *testing.T) {
		inst, _ := kept(t, agent)
		inst.setRuntimeLaunch(tmux.ProgramCodex, nil, time.Time{})
		require.Equal(t, keptCaptureEvidence{}, inst.captureEvidence,
			"a new launch's own evidence supersedes the old runtime's identity")
	})
}

// TestKeptCaptureEvidence_IsDurableAndReverifiedEachDaemonLife pins the
// on-disk contract: the kept pair survives a restart (or the retire's persist
// would erase it, which is how property 2 was lost), the verification does
// not (each daemon life re-proves it), clients never see it, and a row that
// never kept anything writes exactly the record older binaries already read.
func TestKeptCaptureEvidence_IsDurableAndReverifiedEachDaemonLife(t *testing.T) {
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	inst, panes := reattachInstance(t, agent, agent)
	inst.settleReattachVerification(inst.Tabs[0].tmux)
	panes.set(paneRootedAt(agent))
	require.NoError(t, inst.CheckConversationCaptureEvidence())

	data := inst.ToInstanceData()
	require.Zero(t, data.RuntimePID)
	require.Equal(t, agent.PID, data.CaptureRuntimePID)
	require.Equal(t, agent.StartID, data.CaptureRuntimeStartID)
	require.Zero(t, data.ForClients().CaptureRuntimePID, "daemon-side process identity is withheld from clients")
	require.Zero(t, data.ForClients().CaptureRuntimeStartID)

	raw, err := json.Marshal(data.ForStorage())
	require.NoError(t, err)
	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, agent.PID, stored.CaptureRuntimePID, "the storage projection carries the kept identity")
	// Archived, so the load is inert: a live row would run Start(false), whose
	// respawn is a launch boundary that rightly clears the pair, and whose
	// reattach leaves it (no claim for settleReattachVerification to settle).
	reloaded, err := FromInstanceData(InstanceData{
		ID: "reattach", Title: "reattach", Program: tmux.ProgramCodex, Liveness: LiveArchived,
		Path: t.TempDir(),
		Worktree: GitWorktreeData{
			RepoPath: t.TempDir(), WorktreePath: t.TempDir(), SessionName: "reattach", BranchName: "af/reattach",
		},
		CaptureRuntimePID: stored.CaptureRuntimePID, CaptureRuntimeStartID: stored.CaptureRuntimeStartID,
	})
	require.NoError(t, err)
	require.Equal(t, keptCaptureEvidence{pid: agent.PID, startID: agent.StartID}, reloaded.captureEvidence,
		"the kept identity survives a restart, unverified: the next daemon life re-proves it")

	plain, err := json.Marshal((&Instance{Title: "plain"}).ToInstanceData().ForStorage())
	require.NoError(t, err)
	require.NotContains(t, string(plain), "capture_runtime", "a row with nothing kept writes no new key")
}

// TestCaptureAgentConversation_WithoutEvidenceEndsTheAttempt is the #7124
// review's F2. Without a verified agent process the diff cannot attribute
// anything, so a bound capture must return at once rather than re-walk the
// shared store and re-read every new rollout's header for its whole absent
// window — every rearm, for the runtime's life.
func TestCaptureAgentConversation_WithoutEvidenceEndsTheAttempt(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, inst *Instance)
	}{
		{"no launch record", func(*testing.T, *Instance) {}},
		{"kept identity not yet re-verified", func(t *testing.T, inst *Instance) {
			p := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
				"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
			inst.captureEvidence = keptCaptureEvidence{pid: p.PID, startID: p.StartID}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			workDir := t.TempDir()
			snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
			time.Sleep(20 * time.Millisecond)
			split := writeCodexRolloutFileWithCwd(t, codexHome,
				"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", workDir)
			inst, _ := activePaneInstance(t, startRolloutHolder(t, split))
			tc.seed(t, inst)

			const window = 2 * time.Second
			start := time.Now()
			conv, err := CaptureAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap), window)
			elapsed := time.Since(start)
			require.NoError(t, err)
			require.False(t, conv.HasID())
			require.Less(t, elapsed, window/4,
				"an attempt that cannot attribute anything must not spend its %s window polling the store", window)
		})
	}
}

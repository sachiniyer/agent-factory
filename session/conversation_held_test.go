package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
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

// TestHeldAgentConversation_NamesTheHeldRolloutItsBaselineAlreadyHas is the
// restart / mid-swap shape: the replacement's mission minted its rollout
// BEFORE the capture that is now armed took its baseline. The file is in the
// before-set, so the new-since-baseline diff can never see it — which is why
// the daemon's loop asks the runtime when the diff comes back empty — and the
// runtime holding it open is positive evidence on its own.
func TestHeldAgentConversation_NamesTheHeldRolloutItsBaselineAlreadyHas(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	// The late baseline: the daemon restarted, or the poll's deferred arm
	// claimed the replacement's generation after the mission landed.
	snap := heldTestInstance(t, own).BindConversationCaptureRuntime(
		beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir))

	diff, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 100*time.Millisecond)
	require.NoError(t, err)
	require.False(t, diff.HasID(), "the baseline diff alone is blind to a rollout its baseline lists")

	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID,
		"the rollout the runtime holds open is its conversation even when a late baseline already lists it")
	require.Equal(t, tmux.ProgramCodex, conv.Agent)
	require.Equal(t, ConversationCaptureCodexRollout, conv.CaptureKind)
}

// TestHeldAgentConversation_NamesAResumedThreadOutsideTheScanWindow is the
// `/resume` shape: the pane resumed a thread minted weeks ago, whose day
// directory the bounded store scan never descends into (#6845). The held-open
// answer names the file directly, so no store walk is needed.
func TestHeldAgentConversation_NamesAResumedThreadOutsideTheScanWindow(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	old := time.Now().Add(-30 * 24 * time.Hour)
	resumed := writeCodexRolloutAt(t, codexHome, workDir, old, codexRolloutName(old, heldTestOwnID))
	snap := heldTestInstance(t, resumed).BindConversationCaptureRuntime(
		beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir))

	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID,
		"a resumed thread the runtime holds must be recorded wherever it sits in the store")
}

// TestHeldAgentConversation_AdoptsOnlyTheRuntimesOwnTopLevelThread pins what
// held-open evidence may and may not name. The runtime legitimately holds its
// sub-agents' rollouts too (#6766), a thread another worktree's directory
// names is never this session's, and a file outside the snapshot's store is
// not a rollout of it however it is spelled.
func TestHeldAgentConversation_AdoptsOnlyTheRuntimesOwnTopLevelThread(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	sub := writeCodexRolloutFileWithSource(t, codexHome,
		"rollout-2026-11-11T14-12-24-01a1ccab-5a2b-7ce3-a669-aae897bc1692.jsonl", workDir,
		map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": heldTestOwnID}}})
	elsewhere := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-04-01a1cc95-ff0f-7661-972c-0fd1ee1874fd.jsonl", t.TempDir())
	otherStore := writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T13-58-55-01a1cc9f-026d-7b40-aa23-cb0eb41de63e.jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	inst := heldTestInstance(t, own, sub, elsewhere, otherStore, "/dev/null")
	conv, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID,
		"of everything the runtime holds, only its own top-level thread in this worktree and store is the conversation")

	// Without the top-level thread, nothing else qualifies: a runtime that
	// holds only sub-agents and strangers answers "nothing to record".
	inst = heldTestInstance(t, sub, elsewhere, otherStore)
	conv, err = HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.False(t, conv.HasID())
}

// TestHeldAgentConversation_TwoHeldThreadsAreAmbiguousNotGuessed: if the
// runtime holds two top-level rollouts for the worktree, af cannot tell which
// thread the pane shows, so it says so rather than picking one.
func TestHeldAgentConversation_TwoHeldThreadsAreAmbiguousNotGuessed(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	a := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-"+heldTestSiblingID+".jsonl", workDir)
	b := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	inst := heldTestInstance(t, a, b)

	conv, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.ErrorIs(t, err, ErrCodexConversationAmbiguous)
	require.False(t, conv.HasID())
	require.Contains(t, err.Error(), heldTestOwnID, "the ambiguity names both threads so an operator can repair it")
	require.Contains(t, err.Error(), heldTestSiblingID)
}

// TestHeldAgentConversation_UnclassifiableHeldHeaderIsAnErrorNotAbsence: a
// held rollout whose session_meta cannot be read might be the thread itself,
// so "nothing held" would be a guess. It is reported.
func TestHeldAgentConversation_UnclassifiableHeldHeaderIsAnErrorNotAbsence(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	headerless := writeCodexRolloutFile(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl")
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	inst := heldTestInstance(t, headerless)

	_, err := HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
	require.ErrorIs(t, err, errCodexCaptureUndecided)
}

// TestHeldAgentConversation_UnboundSnapshotAnswersNothing: a channel that can
// only judge candidates (the fixture shape) or no channel at all never invents
// a held set, and a non-Codex provider has no store to ask about.
func TestHeldAgentConversation_UnboundSnapshotAnswersNothing(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.False(t, conv.HasID(), "an unbound snapshot has no runtime to ask")
	conv, err = HeldAgentConversation(tmux.ProgramCodex, bindRuntimeHeldFiles(snap, map[string]struct{}{own: {}}))
	require.NoError(t, err)
	require.False(t, conv.HasID(), "a judge-only channel exposes no held set")
	conv, err = HeldAgentConversation(tmux.ProgramClaude, heldTestInstance(t, own).BindConversationCaptureRuntime(snap))
	require.NoError(t, err)
	require.False(t, conv.HasID(), "only Codex has an inspectable store")
}

// TestHeldAgentConversation_RealProcessHoldingItsRollout runs the production
// channel end to end, with no override: a real process holds the rollout
// open, the instance's runtime evidence names that process, and the held
// query reads its descriptors through proctree — /proc on Linux,
// proc_pidfdinfo on darwin, where a temp store's /var spelling and the
// kernel's /private/var answer must still meet (#7101 lift condition).
func TestHeldAgentConversation_RealProcessHoldingItsRollout(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	holder := exec.Command("tail", "-f", own)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
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

	var conv AgentConversationData
	var lastErr error
	require.Eventually(t, func() bool {
		conv, lastErr = HeldAgentConversation(tmux.ProgramCodex, inst.BindConversationCaptureRuntime(snap))
		return lastErr == nil && conv.ID == heldTestOwnID
	}, 5*time.Second, 20*time.Millisecond,
		"the rollout a real runtime holds open must be named: last result %+v, %v", conv, lastErr)
}

// TestExactResumeFreshStartMessageNamesARefusal: when a restore refuses the
// session's recorded id and starts fresh, the warning says so — session, id
// and reason — instead of claiming nothing was recorded (#7101: the incident's
// log read "no recorded conversation id" for a session that had one).
func TestExactResumeFreshStartMessageNamesARefusal(t *testing.T) {
	refused := tmux.ExactResumeFreshStartMessage("af_bc8bf732_docs-to-docu", tmux.ExactResumeRefusal{
		Session: "docs-to-docu", ID: heldTestSiblingID, Reason: staleCarryReason,
	})
	require.Contains(t, refused, `"docs-to-docu"`)
	require.Contains(t, refused, heldTestSiblingID)
	require.Contains(t, refused, staleCarryReason)
	require.NotContains(t, refused, "no recorded conversation id")

	absent := tmux.ExactResumeFreshStartMessage("af_bc8bf732_docs-to-docu", tmux.ExactResumeRefusal{})
	require.Contains(t, absent, "no recorded conversation id to resume")
}

// TestFollowAgentConversationForRuntimeRefusesWhatItMustNotMove pins the
// follow commit's compare-and-set: it moves the record only from the exact id
// the caller judged, only for the live generation, and never while a swap,
// its staged launch plan, a handoff mission, or any operation owns the record
// (#7101 lift condition 3).
func TestFollowAgentConversationForRuntimeRefusesWhatItMustNotMove(t *testing.T) {
	judged := AgentConversationData{Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242"}
	next := AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestOwnID}

	frozen := map[string]func(*Instance){
		"pending account swap": func(i *Instance) { i.pendingAccountSwap = &AccountSwapData{Manual: true} },
		"staged launch plan":   func(i *Instance) { i.accountSwapLaunch = &accountSwapLaunchPlan{} },
		"pending handoff":      func(i *Instance) { i.pendingHandoffMission = "continue" },
		"operation in flight":  func(i *Instance) { i.inFlightOp = OpReplacing },
	}
	for name, freeze := range frozen {
		t.Run(name, func(t *testing.T) {
			inst := retireTestInstance(t)
			token := inst.AgentRuntimeToken()
			freeze(inst)
			require.NotEmpty(t, inst.ConversationFollowFrozen())
			require.False(t, inst.FollowAgentConversationForRuntime(token, judged, next))
			require.Equal(t, judged.ID, inst.AgentConversation().ID)
		})
	}

	t.Run("record changed since judged", func(t *testing.T) {
		inst := retireTestInstance(t)
		token := inst.AgentRuntimeToken()
		repaired := AgentConversationData{Agent: tmux.ProgramCodex, ID: "5299e00d-1111-4222-8333-f7045e07a242"}
		inst.SetAgentConversation(repaired)
		require.False(t, inst.FollowAgentConversationForRuntime(token, judged, next))
		require.Equal(t, repaired.ID, inst.AgentConversation().ID)
	})
	t.Run("runtime replaced since judged", func(t *testing.T) {
		inst := retireTestInstance(t)
		token := inst.AgentRuntimeToken()
		inst.noteAgentRuntimeReplaced()
		require.False(t, inst.FollowAgentConversationForRuntime(token, judged, next))
		require.Equal(t, judged.ID, inst.AgentConversation().ID)
	})
	t.Run("unfrozen follows", func(t *testing.T) {
		inst := retireTestInstance(t)
		require.Empty(t, inst.ConversationFollowFrozen())
		require.True(t, inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), judged, next))
		require.Equal(t, next.ID, inst.AgentConversation().ID)
	})
}

// TestHeldAgentConversation_HolderFastPathFollowsAThreadSwitch runs the
// steady-state fast path on real processes: the first answer learns which
// member holds the thread, and when that member moves to another thread
// (`/new`) its own descriptors disagree with the remembered answer, so the
// query falls through to the full walk and names the new thread.
func TestHeldAgentConversation_HolderFastPathFollowsAThreadSwitch(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	first := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-"+heldTestSiblingID+".jsonl", workDir)
	second := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	fifo := filepath.Join(t.TempDir(), "switch")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	// Holds the first thread on fd 3 until told to switch, then holds only
	// the second — the shape of a Codex recorder moving threads.
	holder := exec.Command("sh", "-c", `exec 3<"$1"; read _ < "$3"; exec 3<&-; exec 3<"$2"; exec sleep 30`,
		"sh", first, second, fifo)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	})
	var root proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(holder.Process.Pid)
		root = p
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	inst := &Instance{}
	inst.runtimePID, inst.runtimeStartID = root.PID, root.StartID
	bound := inst.BindConversationCaptureRuntime(snap)

	require.Eventually(t, func() bool {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, bound)
		return err == nil && conv.ID == heldTestSiblingID
	}, 5*time.Second, 20*time.Millisecond, "the first thread must be named")
	inst.codexAttrMu.Lock()
	learned := inst.codexHolder
	inst.codexAttrMu.Unlock()
	require.Equal(t, root.PID, learned.pid, "the member that holds the thread is remembered")
	require.Equal(t, heldTestSiblingID, learned.id)

	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = writer.WriteString("go\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	var conv AgentConversationData
	require.Eventually(t, func() bool {
		conv, err = HeldAgentConversation(tmux.ProgramCodex, bound)
		return err == nil && conv.ID == heldTestOwnID
	}, 5*time.Second, 20*time.Millisecond,
		"a holder that moved threads must not pin the old answer: last %+v, %v", conv, err)
}

// TestHeldAgentConversation_FastPathNeverOverridesAMovedRecord is the #7120
// review case. The holder fast path is only equivalent to the full walk while
// the record is the holder's own answer: once an operator repaired the record
// to a thread ANOTHER pane member holds, answering from the cached holder
// would re-point the repair, where the full walk sees both threads and keeps
// it. So a record that moved forces the full walk.
func TestHeldAgentConversation_FastPathNeverOverridesAMovedRecord(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	first := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-"+heldTestSiblingID+".jsonl", workDir)
	second := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	fifo := filepath.Join(t.TempDir(), "spawn")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	// The root holds the first thread throughout; on the signal it starts a
	// second member that holds only the second thread.
	holder := exec.Command("sh", "-c", `exec 3<"$1"; read _ < "$3"; (exec 3<&- 4<"$2"; exec sleep 30) & wait`,
		"sh", first, second, fifo)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, holder.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-holder.Process.Pid, syscall.SIGKILL)
		_, _ = holder.Process.Wait()
	})
	var root proctree.Process
	require.Eventually(t, func() bool {
		p, err := proctree.Lookup(holder.Process.Pid)
		root = p
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	inst := &Instance{Tabs: []*Tab{{}}}
	inst.runtimePID, inst.runtimeStartID = root.PID, root.StartID
	bound := inst.BindConversationCaptureRuntime(snap)
	inst.SetAgentConversation(AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestSiblingID})

	require.Eventually(t, func() bool {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, bound)
		return err == nil && conv.ID == heldTestSiblingID
	}, 5*time.Second, 20*time.Millisecond, "the first thread must be named and its holder learned")

	writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = writer.WriteString("go\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.Eventually(t, func() bool {
		snapshot, err := proctree.Snapshot()
		return err == nil && len(proctree.TreeOf(snapshot, root.PID)) >= 2
	}, 5*time.Second, 20*time.Millisecond, "the second member must start")

	// The operator repairs the record to the thread the second member holds.
	inst.SetAgentConversation(AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestOwnID})
	require.Eventually(t, func() bool {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, bound)
		require.NotEqual(t, heldTestSiblingID, conv.ID,
			"the cached holder's answer must never re-point a record that moved")
		return errors.Is(err, ErrCodexConversationAmbiguous)
	}, 5*time.Second, 20*time.Millisecond,
		"with two threads held, the full walk reports the ambiguity and the repair stands")
}

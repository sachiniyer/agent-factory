package session

import (
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
	snap.runtime = &codexRuntimeAttribution{attribute: func(candidates []string) ([]string, bool) {
		return nil, true
	}}

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
	verdicts := make(map[string]codexRolloutVerdict)
	var mu sync.Mutex
	snap.runtime = &codexRuntimeAttribution{attribute: func(candidates []string) ([]string, bool) {
		return judgeCodexRollouts(func() (map[string]struct{}, error) {
			calls++
			return map[string]struct{}{}, nil
		}, verdicts, &mu, candidates)
	}}
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
	// see — in its OWN session, so the member walk enumerates just this
	// runtime rather than every process the test suite's shared session
	// happens to contain.
	holder := exec.Command("tail", "-f", heldFile)
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
	var lastErr error
	require.Eventually(t, func() bool {
		_, heldFor, err := inst.codexRuntimeJudgement()
		if err != nil {
			lastErr = err
			return false
		}
		paths, err := heldFor()
		if err != nil {
			lastErr = err
			return false
		}
		held = paths
		return true
	}, 5*time.Second, 20*time.Millisecond, "the pane-member walk must resolve: %v", lastErr)
	resolved, err := filepath.EvalSymlinks(heldFile)
	require.NoError(t, err)
	_, ok := held[resolved]
	if !ok {
		_, ok = held[heldFile]
	}
	require.True(t, ok, "a file open in the runtime's own process set must be found")
}

// TestCodexAttributionVerdictsSurviveRebind is the deferred-loop shape #6847's
// review demands: captureAgentConversation re-binds a fresh attribution object
// onto the stored snapshot on EVERY attempt, so a verdict that lived only on
// the snapshot's channel would re-walk the process table per rearm forever.
// The memo lives on the instance, keyed to the resolved generation — a
// rejected sibling is walked once and never again.
func TestCodexAttributionVerdictsSurviveRebind(t *testing.T) {
	inst := &Instance{}
	walks := 0
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		walks++
		// The runtime holds nothing: the same-cwd candidate below is a
		// sibling's rollout, judged foreign forever.
		return map[string]struct{}{}, nil
	})

	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)

	for attempt := 0; attempt < 3; attempt++ {
		// Each attempt binds the armed snapshot fresh, exactly as the
		// deferred loop's captureAgentConversation does on every rearm.
		conv, retry, err := captureCodexConversationOnce(inst.BindConversationCaptureRuntime(snap))
		require.NoError(t, err)
		require.True(t, retry)
		require.False(t, conv.HasID(), "a foreign rollout must never be adopted, on any attempt")
	}
	require.Equal(t, 1, walks,
		"a sibling judged foreign must not re-walk the runtime on later attempts")
}

// TestCodexAttributionDoesNotContaminateReplacementGeneration: a deferred
// loop displaced mid-attempt may overlap its replacement (#6907's rearm
// semantics). The old attempt's verdicts must land in ITS generation's map —
// a stale map that is then discarded — never in the replacement's cache.
func TestCodexAttributionDoesNotContaminateReplacementGeneration(t *testing.T) {
	oldHolder := exec.Command("sleep", "30")
	require.NoError(t, oldHolder.Start())
	t.Cleanup(func() { _ = oldHolder.Process.Kill() })
	newHolder := exec.Command("sleep", "30")
	require.NoError(t, newHolder.Start())
	t.Cleanup(func() { _ = newHolder.Process.Kill() })

	inst := &Instance{}
	setRoot := func(pid int) {
		var root proctree.Process
		require.Eventually(t, func() bool {
			p, err := proctree.Lookup(pid)
			if err == nil {
				root = p
			}
			return err == nil
		}, 5*time.Second, 20*time.Millisecond, "holder %d must be visible", pid)
		inst.mu.Lock()
		inst.runtimePID, inst.runtimeStartID = root.PID, root.StartID
		inst.mu.Unlock()
	}
	setRoot(oldHolder.Process.Pid)

	const oldCand = "/tmp/6847-old-generation-rollout.jsonl"
	const newCand = "/tmp/6847-new-generation-rollout.jsonl"
	release := make(chan struct{})
	var callsMu sync.Mutex
	heldCalls := 0
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		callsMu.Lock()
		heldCalls++
		n := heldCalls
		callsMu.Unlock()
		if n == 1 {
			<-release // the displaced attempt blocks inside its descriptor walk
			return map[string]struct{}{oldCand: {}}, nil
		}
		return map[string]struct{}{newCand: {}}, nil
	})

	type result struct {
		attributed   []string
		inconclusive bool
	}
	oldDone := make(chan result, 1)
	go func() {
		a, inc := inst.attributeCodexRolloutPaths([]string{oldCand})
		oldDone <- result{a, inc}
	}()
	require.Eventually(t, func() bool {
		callsMu.Lock()
		defer callsMu.Unlock()
		return heldCalls == 1
	}, 5*time.Second, 10*time.Millisecond, "the old attempt must be inside its walk")

	// The runtime respawns: the replacement generation gets its own cache.
	setRoot(newHolder.Process.Pid)
	attributed, inconclusive := inst.attributeCodexRolloutPaths([]string{newCand})
	require.False(t, inconclusive)
	require.Equal(t, []string{newCand}, attributed)

	close(release)
	old := <-oldDone
	require.True(t, old.inconclusive, "a displaced attempt's result must be discarded")

	inst.codexAttrMu.Lock()
	defer inst.codexAttrMu.Unlock()
	require.Len(t, inst.codexAttrVerdicts, 1,
		"the replacement generation's cache must hold only its own verdicts")
	require.True(t, inst.codexAttrVerdicts[newCand].ours,
		"the replacement generation's cache must hold only its own verdicts")
}

// TestCodexAttributionResolvesStoreAliases: CODEX_HOME reached through a
// symlink yields candidates under the alias while the kernel's descriptor
// answers carry the resolved path — the runtime's own rollout must still
// attribute (#6847 review).
func TestCodexAttributionResolvesStoreAliases(t *testing.T) {
	realHome := t.TempDir()
	alias := filepath.Join(t.TempDir(), "codex-alias")
	require.NoError(t, os.Symlink(realHome, alias))
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(alias, workDir)

	name := "rollout-2026-07-06T10-17-40-019f386f-7206-7fc2-803b-f7045e07a242.jsonl"
	aliasPath := writeCodexRolloutFileWithCwd(t, alias, name, workDir)
	// What the runtime's descriptors report: the kernel-resolved path.
	realPath := filepath.Join(codexRolloutDayDir(realHome, time.Now()), name)
	require.NotEqual(t, aliasPath, realPath, "the fixture must actually exercise the alias")
	snap = bindRuntimeHeldFiles(snap, map[string]struct{}{realPath: {}})

	conv, _, err := captureCodexConversationOnce(snap)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID,
		"the runtime's own rollout attributes through a store alias")
}

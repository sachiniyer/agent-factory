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

// mutableHeldOverride is the override shape a test uses to model a runtime
// whose held set changes mid-life: Codex /new retires the old thread's write
// descriptor during a ~60s unload delay while the new thread's recorder is
// already open, so a single pane genuinely holds two same-cwd rollouts for
// roughly a minute and then only one. It counts descriptor walks so a test can
// assert the cache was re-consulted rather than reusing a stale verdict.
type mutableHeldOverride struct {
	mu    sync.Mutex
	held  map[string]struct{}
	walks int
}

func (m *mutableHeldOverride) snapshot() (map[string]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.walks++
	cp := make(map[string]struct{}, len(m.held))
	for p := range m.held {
		cp[p] = struct{}{}
	}
	return cp, nil
}

func (m *mutableHeldOverride) release(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, path)
}

// TestCodexAttributionCachedPositiveIsRejudgedWhenHeldSetShrinks is the wedge
// regression: a single descriptor walk that catches two same-cwd rollouts held
// together used to mint two permanent positives (a positive verdict was cached
// forever and never re-verified against the current held set), so every later
// capture attempt returned the same hard, non-retriable ambiguity — even after
// the runtime released one of the two rollouts. With the fix, a cached positive
// ages out on the same cadence as a negative and is re-walked, so once the
// runtime holds only one rollout the next attempt attributes it and records
// the thread.
func TestCodexAttributionCachedPositiveIsRejudgedWhenHeldSetShrinks(t *testing.T) {
	prev := codexAttributionRejudgeInterval
	codexAttributionRejudgeInterval = 10 * time.Millisecond
	t.Cleanup(func() { codexAttributionRejudgeInterval = prev })

	codexHome := t.TempDir()
	workDir := t.TempDir()
	baseSnap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	p1 := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	p2 := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)

	held := &mutableHeldOverride{held: map[string]struct{}{p1: {}, p2: {}}}
	verdicts := make(map[string]codexRolloutVerdict)
	var mu sync.Mutex
	bind := func() ConversationCaptureSnapshot {
		snap := baseSnap
		snap.runtime = &codexRuntimeAttribution{attribute: func(candidates []string) ([]string, bool) {
			return judgeCodexRollouts(held.snapshot, verdicts, &mu, candidates)
		}}
		return snap
	}

	// First attempt: the runtime holds both rollouts. Both are judged ours
	// and cached; capture reports a non-retriable ambiguity.
	_, retry, err := captureCodexConversationOnce(bind())
	require.ErrorContains(t, err, "2 new rollout files are held open by the session's runtime")
	require.False(t, retry, "two genuinely-held rollouts are still ambiguous inside the overlap window")
	require.Equal(t, 1, held.walks, "the first attempt walks the held set once to judge both candidates")

	// The runtime releases p1: only p2 is held now (the /new transition).
	held.release(p1)
	// Within the re-judge cadence the cached positives are still reused —
	// the cache is the optimization, not the bug — so the ambiguity
	// persists momentarily.
	_, _, err = captureCodexConversationOnce(bind())
	require.ErrorContains(t, err, "ambiguous", "inside the re-judge cadence the cache is still reused")
	require.Equal(t, 1, held.walks, "no re-walk happens before the cadence elapses")

	// Past the cadence the cached positives age out and are re-walked
	// against the current held set, which now contains only p2.
	time.Sleep(30 * time.Millisecond)
	conv, retry, err := captureCodexConversationOnce(bind())
	require.NoError(t, err, "once the runtime holds only one rollout, capture must attribute it")
	require.False(t, retry, "a single attributed rollout is a final answer")
	require.True(t, conv.HasID())
	require.Equal(t, "019f386f-75b6-7f68-88e3-6d5e1f15bb6a", conv.ID,
		"the surviving rollout — not the released one — is recorded")
	require.Equal(t, 2, held.walks, "the cached positives were re-walked after the cadence elapsed")
}

// TestCodexAttributionCachedPositiveRejudgesOnInstanceAcrossRearms runs the
// same wedge through the production instance-side binding
// (Instance.attributeCodexRolloutPaths, which memoizes verdicts on the Instance
// keyed to the resolved runtime generation). The deferred capture loop re-binds
// a fresh attribution object on every rearm; without the fix the cached
// positives on the instance pinned the ambiguity for the life of the
// generation. With the fix, a rearm past the re-judge cadence re-walks the held
// set and attributes the surviving rollout.
func TestCodexAttributionCachedPositiveRejudgesOnInstanceAcrossRearms(t *testing.T) {
	prev := codexAttributionRejudgeInterval
	codexAttributionRejudgeInterval = 10 * time.Millisecond
	t.Cleanup(func() { codexAttributionRejudgeInterval = prev })

	codexHome := t.TempDir()
	workDir := t.TempDir()
	baseSnap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	p1 := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	p2 := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)

	held := &mutableHeldOverride{held: map[string]struct{}{p1: {}, p2: {}}}
	inst := &Instance{}
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		return held.snapshot()
	})

	// First rearm: both held → ambiguous, both cached positive on the
	// instance under the override's generation.
	_, _, err := captureCodexConversationOnce(inst.BindConversationCaptureRuntime(baseSnap))
	require.ErrorContains(t, err, "ambiguous")
	require.Equal(t, 1, held.walks)

	// The runtime releases p1.
	held.release(p1)
	time.Sleep(30 * time.Millisecond)

	// The daemon loop re-binds the armed snapshot fresh on every rearm;
	// the verdict cache stays on the instance, so a rearm past the cadence
	// must re-walk and attribute the surviving rollout.
	conv, _, err := captureCodexConversationOnce(inst.BindConversationCaptureRuntime(baseSnap))
	require.NoError(t, err)
	require.Equal(t, "019f386f-75b6-7f68-88e3-6d5e1f15bb6a", conv.ID)
	require.Equal(t, 2, held.walks, "the instance's cached positives were re-walked after the cadence")
}

// TestCodexAttributionFreshPositiveIsStillMemoizedWithinCadence pins the
// optimization the cache exists for: a freshly-judged positive is reused
// within one re-judge cadence, so the descriptor walk is not re-run on every
// 50ms poll. The fix re-judges positives on a cadence; it must not collapse
// into re-walking every poll.
func TestCodexAttributionFreshPositiveIsStillMemoizedWithinCadence(t *testing.T) {
	prev := codexAttributionRejudgeInterval
	codexAttributionRejudgeInterval = 250 * time.Millisecond
	t.Cleanup(func() { codexAttributionRejudgeInterval = prev })

	codexHome := t.TempDir()
	workDir := t.TempDir()
	baseSnap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	own := writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-49-03-019f386f-7206-7fc2-803b-f7045e07a242.jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome,
		"rollout-2026-11-11T13-50-27-019f386f-75b6-7f68-88e3-6d5e1f15bb6a.jsonl", workDir)

	held := &mutableHeldOverride{held: map[string]struct{}{own: {}}}
	verdicts := make(map[string]codexRolloutVerdict)
	var mu sync.Mutex
	bind := func() ConversationCaptureSnapshot {
		snap := baseSnap
		snap.runtime = &codexRuntimeAttribution{attribute: func(candidates []string) ([]string, bool) {
			return judgeCodexRollouts(held.snapshot, verdicts, &mu, candidates)
		}}
		return snap
	}

	for i := 0; i < 3; i++ {
		conv, _, err := captureCodexConversationOnce(bind())
		require.NoError(t, err)
		require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
	}
	require.Equal(t, 1, held.walks,
		"a fresh positive is reused within the re-judge cadence, not re-walked every poll")
}

package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #7378 (the #7370 review's R4): the CODEX_HOME reads left unbounded on the
// capture, attribution, receipt, restore and carry paths. Each now runs
// inside fsprobe's bound, and a lookup that does not answer is "could not
// look", mapped as #7370 settled. Capture waits and never records from a
// partial view. The restore resumes with the no-evidence answer and retires
// nothing. The carry refuses, and never as stale from a read that did not
// answer.

// lookupWedge blocks every lookup of a path match accepts until the test
// ends, and bounds af's probes at 50ms.
func lookupWedge(t *testing.T, match func(string) bool) func(string) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		// Let any flight still blocked in a seam finish before the real
		// lookups come back.
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
	return func(path string) {
		if match(path) {
			<-release
		}
	}
}

// wedgeStatMatching makes every fsprobe.Stat of a path match accepts block.
func wedgeStatMatching(t *testing.T, match func(string) bool) {
	t.Helper()
	wait := lookupWedge(t, match)
	t.Cleanup(fsprobe.SetStatPathForTest(func(p string) (os.FileInfo, error) {
		wait(p)
		return os.Stat(p)
	}))
}

// wedgeLstatMatching makes every fsprobe.Lstat of a path match accepts block.
func wedgeLstatMatching(t *testing.T, match func(string) bool) {
	t.Helper()
	wait := lookupWedge(t, match)
	t.Cleanup(fsprobe.SetLstatForTest(func(p string) (os.FileInfo, error) {
		wait(p)
		return os.Lstat(p)
	}))
}

// wedgeEvalSymlinksMatching makes every fsprobe symlink resolution of a path
// match accepts block.
func wedgeEvalSymlinksMatching(t *testing.T, match func(string) bool) {
	t.Helper()
	wait := lookupWedge(t, match)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(p string) (string, error) {
		wait(p)
		return filepath.EvalSymlinks(p)
	}))
}

func pathIs(path string) func(string) bool {
	return func(p string) bool { return p == path }
}

func baseIs(name string) func(string) bool {
	return func(p string) bool { return filepath.Base(p) == name }
}

type captureOnceAnswer struct {
	conv AgentConversationData
	err  error
}

func captureOnceWithinBound(t *testing.T, snap ConversationCaptureSnapshot) captureOnceAnswer {
	t.Helper()
	return withinBound(t, func() captureOnceAnswer {
		conv, _, err := captureCodexConversationOnce(snap)
		return captureOnceAnswer{conv, err}
	})
}

// requireCaptureWaits asserts capture's could-not-look answer: nothing
// recorded, and the undecided error the settle waits on.
func requireCaptureWaits(t *testing.T, got captureOnceAnswer, msg string) {
	t.Helper()
	require.False(t, got.conv.HasID(), "%s: recorded %q", msg, got.conv.ID)
	require.True(t, errors.Is(got.err, errCodexCaptureUndecided), "%s: capture waits: %v", msg, got.err)
}

// Site 1, the capture diff. Each new rollout is statted to drop one older
// than the snapshot. A stat that does not answer leaves its age unknown, so
// capture waits on it. It never adopts the rollout as though the stat had
// said it was new.
func TestCodexCaptureWaitsOnANewRolloutWhoseStatDoesNotAnswer(t *testing.T) {
	codexHome := t.TempDir()
	workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	own := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	held := map[string]struct{}{own: {}}

	conv, _, err := captureCodexConversationOnce(bindRuntimeHeldFiles(snap, held))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: the new rollout the runtime holds is adopted")

	wedgeStatMatching(t, pathIs(own))
	requireCaptureWaits(t, captureOnceWithinBound(t, bindRuntimeHeldFiles(snap, held)),
		"a new rollout whose age af could not read")
}

// Site 2, runtime attribution. A store reached through a symlinked
// CODEX_HOME lists rollouts under the alias, while the runtime's descriptors
// carry the resolved path, so attribution matches them by (device, inode) or
// by the alias's resolution (#6847). When the held path's identity and the
// candidate's resolution both fail to answer, attribution cannot say the
// runtime does not hold the candidate. It is inconclusive, so capture waits.
// It is never a cached "not held", which would let a second candidate be
// adopted from half the evidence.
func TestCodexAttributionIsInconclusiveWhenAnAliasLookupDoesNotAnswer(t *testing.T) {
	base := testguard.CanonicalTempDir(t)
	home, alias, workDir := filepath.Join(base, "home"), filepath.Join(base, "alias"), filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	require.NoError(t, os.Symlink(home, alias))
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(alias, workDir)
	real := writeCodexRolloutFileWithCwd(t, home, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	rel, err := filepath.Rel(home, real)
	require.NoError(t, err)
	aliased := filepath.Join(alias, rel)
	held := map[string]struct{}{real: {}}

	conv, _, err := captureCodexConversationOnce(bindRuntimeHeldFiles(snap, held))
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: the runtime's resolved spelling attributes the aliased candidate")

	wedgeStatMatching(t, pathIs(real))
	wedgeEvalSymlinksMatching(t, pathIs(aliased))
	requireCaptureWaits(t, captureOnceWithinBound(t, bindRuntimeHeldFiles(snap, held)),
		"a candidate af could not match to what the runtime holds")
}

// codexReceiptLayout is a resumed rollout tracked as a receipt target, with the
// briefing appended after its boundary.
func codexReceiptLayout(t *testing.T, prompt string) (ConversationCaptureSnapshot, string) {
	t.Helper()
	codexHome := t.TempDir()
	workDir := t.TempDir()
	tracked := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	require.True(t, snap.TrackCodexReceiptRollout(tracked))
	turn := `{"type":"event_msg","payload":{"type":"user_message","message":"` + prompt + `"}}` + "\n"
	f, err := os.OpenFile(tracked, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(turn)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return snap, tracked
}

type receiptOnceAnswer struct {
	received, undecided bool
	err                 error
}

func receiptOnceWithinBound(t *testing.T, snap ConversationCaptureSnapshot, prompt string) receiptOnceAnswer {
	t.Helper()
	return withinBound(t, func() receiptOnceAnswer {
		received, undecided, _, err := codexPromptReceiptOnce(snap, prompt)
		return receiptOnceAnswer{received, undecided, err}
	})
}

// Site 3, the receipt scan. A tracked rollout's stat, open and read run inside
// the bound. One that does not answer is undecided, so the receipt wait keeps
// polling. It is neither a receipt nor an error that ends the wait.
func TestCodexPromptReceiptWaitsWhenAReceiptReadDoesNotAnswer(t *testing.T) {
	const prompt = "the af briefing"
	t.Run("every read answers", func(t *testing.T) {
		snap, _ := codexReceiptLayout(t, prompt)
		got := receiptOnceWithinBound(t, snap, prompt)
		require.NoError(t, got.err)
		require.True(t, got.received, "precondition: the briefing past the boundary is the receipt")
	})
	t.Run("the rollout's stat does not answer", func(t *testing.T) {
		snap, tracked := codexReceiptLayout(t, prompt)
		wedgeStatMatching(t, pathIs(tracked))
		got := receiptOnceWithinBound(t, snap, prompt)
		require.NoError(t, got.err, "a read that did not answer does not end the wait")
		require.False(t, got.received)
		require.True(t, got.undecided, "the receipt is undecided")
	})
	t.Run("the rollout's open does not answer", func(t *testing.T) {
		snap, tracked := codexReceiptLayout(t, prompt)
		stallCodexRolloutOpens(t, pathIs(tracked))
		got := receiptOnceWithinBound(t, snap, prompt)
		require.NoError(t, got.err, "a read that did not answer does not end the wait")
		require.False(t, got.received)
		require.True(t, got.undecided, "the receipt is undecided")
	})
}

// setCodexReceiptScanCapForTest lowers the receipt scan's bound until restore.
func setCodexReceiptScanCapForTest(limit int64) (restore func()) {
	prev := codexReceiptScanCapOverride.Swap(limit)
	return func() { codexReceiptScanCapOverride.Store(prev) }
}

// The receipt scan reads the rollout from its boundary, never the whole file:
// the briefing is the first turn after the boundary, so a scan bounded past it
// still finds it, and a transcript that grows without limit cannot make one
// poll read without limit.
func TestCodexPromptReceiptScanIsBounded(t *testing.T) {
	const prompt = "the af briefing"
	restore := setCodexReceiptScanCapForTest(256)
	t.Cleanup(restore)
	snap, _ := codexReceiptLayout(t, prompt)
	got := receiptOnceWithinBound(t, snap, prompt)
	require.NoError(t, got.err)
	require.True(t, got.received, "the briefing just past the boundary is within the bound")

	snap, tracked := codexReceiptLayout(t, "unrelated turn "+strings.Repeat("x", 300))
	f, err := os.OpenFile(tracked, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"` + prompt + `"}}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got = receiptOnceWithinBound(t, snap, prompt)
	require.NoError(t, got.err)
	require.False(t, got.received, "a turn past the bound is not read")
}

// Site 4, receipt tracking. A rollout is tracked at its size when tracking
// begins, the boundary only later turns can cross. A stat that does not answer
// gives no honest boundary, so the rollout is not tracked, as when the stat
// fails: the pinned target reads as unidentified, and a pool member is left
// out.
func TestCodexReceiptTrackingSkipsARolloutWhoseStatDoesNotAnswer(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	tracked := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	pool := snap
	pool.TrackCodexReceiptPool(true)
	require.Len(t, pool.receiptPaths, 1, "precondition: the pool tracks the rollout")

	wedgeStatMatching(t, pathIs(tracked))
	require.False(t, withinBound(t, func() bool { return snap.TrackCodexReceiptRollout(tracked) }),
		"a rollout af could not stat has no honest boundary")
	pool = snap
	withinBound(t, func() bool { pool.TrackCodexReceiptPool(true); return true })
	require.Empty(t, pool.receiptPaths, "a pool member af could not stat is left out")
}

// requireRestoreResumes asserts the restore's no-evidence answer: resume the
// recorded thread, and retire nothing.
func requireRestoreResumes(t *testing.T, inst *Instance, id string) {
	t.Helper()
	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "codex")
		return exactResumeAnswer{program, ok, decline}
	})
	require.True(t, got.ok, "a lookup that did not answer resumes the recorded thread: %+v", got.decline)
	require.Contains(t, got.program, "resume "+id)
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired")
}

// Site 5, the restore's staleness lstats. The recorded rollout and each
// newer one are lstatted for their mtimes. A recorded rollout whose lstat
// does not answer is not gone, which would decline it as missing and retire
// the record. A newer rollout whose lstat does not answer is not skipped as
// absent, which could make the record read as current. Either way nothing was
// attributed, so the restore resumes and retires nothing.
func TestCodexRestoreResumesWhenAStalenessLstatDoesNotAnswer(t *testing.T) {
	layout := func(t *testing.T) *Instance {
		wt := filepath.Join(testguard.CanonicalTempDir(t), "wt")
		require.NoError(t, os.MkdirAll(wt, 0o755))
		inst := codexRestoreGuardLayout(t, ConversationCaptureCodexRollout, wt, wt, wt)
		_, ok, decline := prepareExactResumeConversation(inst, "codex")
		require.False(t, ok, "precondition: the newer rollout in this worktree makes the record stale")
		require.Equal(t, staleCarryReason, decline.reason)
		return inst
	}
	t.Run("the recorded rollout's lstat", func(t *testing.T) {
		inst := layout(t)
		wedgeLstatMatching(t, baseIs(codexBoundRolloutName(codexBoundRecordedID)))
		requireRestoreResumes(t, inst, codexBoundRecordedID)
	})
	t.Run("the newer rollout's lstat", func(t *testing.T) {
		inst := layout(t)
		wedgeLstatMatching(t, baseIs(codexBoundRolloutName(codexBoundNewerID)))
		requireRestoreResumes(t, inst, codexBoundRecordedID)
	})
}

// Site 5, the restore's second look. Where a newer thread is no evidence (a
// session in the user's own checkout), the guard resumes after one more lstat
// of the recorded rollout, which may have moved between forms. An lstat that
// does not answer is not "gone": the rollout is not looked for again and
// declined as missing.
func TestCodexRestoreSecondLookThatDoesNotAnswerIsNotGone(t *testing.T) {
	wt := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	inst := codexRestoreGuardLayout(t, ConversationCaptureCodexRollout, wt, wt, "")
	inst.inPlace = true
	requireRestoreResumes(t, inst, codexBoundRecordedID)

	// The rollout goes after the guard found it. An lstat that answers then
	// sees it gone and looks again; one that does not answer cannot tell.
	onRecordedCodexRolloutLocated(t, func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove %s: %v", path, err)
		}
	})
	wedgeLstatMatching(t, baseIs(codexBoundRolloutName(codexBoundRecordedID)))
	requireRestoreResumes(t, inst, codexBoundRecordedID)
}

// Site 5, the carry. The carry runs the same staleness judge. A recorded
// rollout whose lstat does not answer refuses the carry as a look that did
// not happen (unverified), never as stale.
func TestCodexCarryRefusesWhenTheRecordedRolloutsLstatDoesNotAnswer(t *testing.T) {
	inst := codexCarryLayout(t, true)
	require.Equal(t, carryRefusalCode(staleCarryReason), inst.AccountSwapCarryRefusal(),
		"precondition: the newer rollout refuses the carry as stale")

	wedgeLstatMatching(t, baseIs(filepath.Base(codexRolloutRel(carryTestID))))
	require.Equal(t, carryRefusalCode(carryUncorrelatedReason), withinBound(t, inst.AccountSwapCarryRefusal),
		"a rollout af could not lstat is no evidence the record is stale")
}

// Site 6, the other forms. A recorded thread Codex holds only compressed is
// found by name, then lstatted to pick its newest form. A form whose lstat does
// not answer is not "no such form", which would decline the thread as missing
// and retire the record. The thread is listed, but af cannot date it, so the
// restore gives the no-evidence resume.
func TestCodexRestoreResumesWhenAFormsLstatDoesNotAnswer(t *testing.T) {
	now := time.Now()
	cold := now.Add(-9 * 24 * time.Hour)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	compressed := writeCompressedCodexRollout(t, codexHome, formsThreadID, "", cold, cold)
	writeStoredCodexRollout(t, codexHome, formsOtherID, workDir, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Minute))
	inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
	_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
	require.False(t, ok, "precondition: the compressed thread is judged against the newer one")
	require.Equal(t, staleCarryReason, decline.reason)

	wedgeLstatMatching(t, pathIs(compressed))
	requireRestoreResumes(t, inst, formsThreadID)
}

// wedgeNthLstatMatching makes only the n-th fsprobe.Lstat of a path match
// accepts block until the test ends; every other lstat answers.
func wedgeNthLstatMatching(t *testing.T, n int32, match func(string) bool) {
	t.Helper()
	wait := lookupWedge(t, func(string) bool { return true })
	var seen atomic.Int32
	t.Cleanup(fsprobe.SetLstatForTest(func(p string) (os.FileInfo, error) {
		if match(p) && seen.Add(1) == n {
			wait(p)
		}
		return os.Lstat(p)
	}))
}

// requireCarryRefusedUnverified asserts the live swap's plan refused the
// carry as a look that did not happen.
func requireCarryRefusedUnverified(t *testing.T, inst *Instance) {
	t.Helper()
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	require.NotNil(t, inst.accountSwapLaunch)
	require.Nil(t, inst.accountSwapLaunch.carry, "a rollout af could not look up is not carried")
	require.Equal(t, handoff.CarryUnverified, carryRefusalCode(inst.accountSwapLaunch.carryFallback),
		"it refuses as a look that did not happen: %q", inst.accountSwapLaunch.carryFallback)
}

// #7384 review R1. Where a newer thread does not count, as in a session in
// the user's own checkout, the carry still looks up the recorded rollout.
// Gone, it refuses as missing, whatever a newer thread would mean. A lookup of
// it that does not answer must refuse too (unverified), in the check-ahead
// and in the live plan. The copy reads that file next, unbounded, after the
// outgoing agent stops, so the plan's refusal is what keeps a swap on a dead
// mount failing before teardown.
func TestCodexCarryRefusesWhenTheRecordedRolloutsLstatDoesNotAnswerWhereNewerDoesNotCount(t *testing.T) {
	layout := func(t *testing.T) *Instance {
		inst := codexCarryLayout(t, false)
		inst.inPlace = true
		require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the rollout carries")
		return inst
	}
	recorded := baseIs(filepath.Base(codexRolloutRel(carryTestID)))
	t.Run("the check-ahead", func(t *testing.T) {
		inst := layout(t)
		wedgeLstatMatching(t, recorded)
		require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
	})
	t.Run("the live plan", func(t *testing.T) {
		inst := layout(t)
		wedgeLstatMatching(t, recorded)
		requireCarryRefusedUnverified(t, inst)
	})
}

// #7384 review R3: the staleness walk lstats the recorded rollout again, after
// the judge's own lstat of it answered. That second lstat not answering stops
// the walk where it is, so the walk has not seen the rest: the carry refuses,
// and never carries past a newer thread it did not see.
func TestCodexCarryRefusesWhenTheWalksLstatOfTheRecordedRolloutDoesNotAnswer(t *testing.T) {
	inst := codexCarryLayout(t, true)
	wedgeNthLstatMatching(t, 2, baseIs(filepath.Base(codexRolloutRel(carryTestID))))
	requireCarryRefusedUnverified(t, inst)
}

// #7384 review R2. An unscoped resume pool tracks every rollout in the store.
// One whose tracking stat does not answer has no boundary, so it cannot be
// tracked, but it may hold the briefing too. Here both A and B record it, so
// with every lookup answering the receipt is ambiguous. With A untracked, B's
// lone match must not read as received in any check of that wait. And B is
// still tracked: the pool never shrinks past the member it could not track.
func TestCodexPromptReceiptNeverConfirmsFromAPoolWithAMemberItCouldNotTrack(t *testing.T) {
	const prompt = "the af briefing"
	codexHome := t.TempDir()
	workDir := t.TempDir()
	a := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	b := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-28-"+heldTestSiblingID+".jsonl", workDir)
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	appendTurn := func(path string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"` + prompt + `"}}` + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	all := snap
	all.TrackCodexReceiptPool(true)
	require.Len(t, all.receiptPaths, 2, "precondition: the pool tracks both")

	// A's stat does not answer while the pool is tracked, and answers again
	// once its stuck flight has returned, so every later read answers.
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	restoreStat := fsprobe.SetStatPathForTest(func(p string) (os.FileInfo, error) {
		if p == a {
			<-release
		}
		return os.Stat(p)
	})
	pool := snap
	withinBound(t, func() bool { pool.TrackCodexReceiptPool(true); return true })
	close(release)
	time.Sleep(50 * time.Millisecond)
	restoreStat()
	restoreTimeout()
	var tracked []string
	for _, member := range pool.receiptPaths {
		tracked = append(tracked, member.path)
	}
	require.Equal(t, []string{b}, tracked, "the pool keeps every member it could track")

	appendTurn(a)
	appendTurn(b)
	answered := receiptOnceWithinBound(t, all, prompt)
	require.ErrorIs(t, answered.err, ErrPromptReceiptAmbiguous, "precondition: two members record the briefing")
	for check := 0; check < 2; check++ {
		got := receiptOnceWithinBound(t, pool, prompt)
		require.NoError(t, got.err)
		require.False(t, got.received, "check %d: a lone match is no receipt while a member af could not track may hold another", check)
		require.True(t, got.undecided, "check %d: the receipt is undecided", check)
	}
}

// #7384 review R3: a receipt poll stops at the first scan that does not
// answer, as undecided, rather than wait out a bound for every tracked rollout
// on the same mount in turn.
func TestCodexPromptReceiptPollStopsAtTheFirstScanThatDoesNotAnswer(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	members := map[string]bool{}
	for _, id := range []string{heldTestOwnID, heldTestSiblingID, repairHeldID, repairNextID} {
		members[writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+id+".jsonl", workDir)] = true
	}
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)
	snap.TrackCodexReceiptPool(true)
	require.Len(t, snap.receiptPaths, len(members), "precondition: the pool tracks every member")

	var stalled atomic.Int32
	wedgeStatMatching(t, func(p string) bool {
		if members[p] {
			stalled.Add(1)
			return true
		}
		return false
	})
	got := receiptOnceWithinBound(t, snap, "the af briefing")
	require.NoError(t, got.err)
	require.False(t, got.received)
	require.True(t, got.undecided, "a scan that did not answer leaves the receipt undecided")
	require.EqualValues(t, 1, stalled.Load(), "the poll stops at the first scan that does not answer")
}

// releasableLookupStall installs a seam stand-in, through install, that blocks
// every lookup match accepts until release, and bounds af's probes at 50ms.
// release lets the stuck flights return and waits for them to unpublish, so
// every later lookup answers.
func releasableLookupStall(t *testing.T, install func(wait func(string)) (restore func()), match func(string) bool) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	restoreSeam := install(func(p string) {
		if match(p) {
			<-gate
		}
	})
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(gate)
			time.Sleep(50 * time.Millisecond)
			restoreSeam()
			restoreTimeout()
		})
	}
	t.Cleanup(release)
	return release
}

func statSeam(wait func(string)) func() {
	return fsprobe.SetStatPathForTest(func(p string) (os.FileInfo, error) {
		wait(p)
		return os.Stat(p)
	})
}

func readDirSeam(wait func(string)) func() {
	return fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		wait(dir)
		return os.ReadDir(dir)
	})
}

// receiptRaceLayout is #5471's two config agents in one AF home: a resumed
// rollout A the launch pins or pools, and a concurrent Codex's new rollout C
// in the same launch dir. Both record the same deterministic briefing, so with
// every lookup answering the receipt is ambiguous.
type receiptRaceLayout struct {
	codexHome, workDir, a string
}

const receiptRacePrompt = "the af briefing"

func newReceiptRaceLayout(t *testing.T) receiptRaceLayout {
	t.Helper()
	l := receiptRaceLayout{codexHome: t.TempDir(), workDir: t.TempDir()}
	l.a = writeCodexRolloutFileWithCwd(t, l.codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", l.workDir)
	return l
}

func (l receiptRaceLayout) snapshot() ConversationCaptureSnapshot {
	return beginConversationCaptureAtCodexHomeAndWorkingDir(l.codexHome, l.workDir)
}

// briefBoth appends the briefing to A, and mints C with it.
func (l receiptRaceLayout) briefBoth(t *testing.T) {
	t.Helper()
	c := writeCodexRolloutFileWithCwd(t, l.codexHome, "rollout-2026-11-11T13-50-28-"+heldTestSiblingID+".jsonl", l.workDir)
	for _, path := range []string{l.a, c} {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteString(`{"type":"event_msg","payload":{"type":"user_message","message":"` + receiptRacePrompt + `"}}` + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
}

// requireReceiptNeverConfirms asserts that no check of the wait reads the
// receipt as received from C alone.
func requireReceiptNeverConfirms(t *testing.T, snap ConversationCaptureSnapshot) {
	t.Helper()
	for check := 0; check < 2; check++ {
		got := receiptOnceWithinBound(t, snap, receiptRacePrompt)
		require.NoError(t, got.err)
		require.False(t, got.received, "check %d: a lone match is no receipt while a rollout af could not track may hold another", check)
		require.True(t, got.undecided, "check %d: the receipt is undecided", check)
	}
}

// #7384 review R5 (rc/G). `codex resume <id>` pins A, but A's tracking stat
// does not answer, so A has no boundary and is not tracked. A concurrent C
// recording the same briefing must not confirm the receipt alone: the wait is
// marked as one that could not look at a rollout the launch could append to.
func TestCodexPromptReceiptNeverConfirmsWhenThePinnedTargetsStatDoesNotAnswer(t *testing.T) {
	t.Run("every lookup answers", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		require.True(t, snap.TrackCodexReceiptRollout(l.a))
		l.briefBoth(t)
		require.ErrorIs(t, receiptOnceWithinBound(t, snap, receiptRacePrompt).err, ErrPromptReceiptAmbiguous)
	})
	t.Run("the pinned target's stat does not answer", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		release := releasableLookupStall(t, statSeam, pathIs(l.a))
		require.False(t, withinBound(t, func() bool { return snap.TrackCodexReceiptRollout(l.a) }), "a target af could not stat has no boundary")
		release()
		l.briefBoth(t)
		requireReceiptNeverConfirms(t, snap)
	})
}

// #7384 review R5. An unscoped pool lists the whole store to find every
// rollout the resume could append to. A listing that does not answer tracks
// nothing, and must not leave a concurrent C to confirm the receipt alone.
func TestCodexPromptReceiptNeverConfirmsWhenThePoolsListingDoesNotAnswer(t *testing.T) {
	t.Run("every lookup answers", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		snap.TrackCodexReceiptPool(true)
		l.briefBoth(t)
		require.ErrorIs(t, receiptOnceWithinBound(t, snap, receiptRacePrompt).err, ErrPromptReceiptAmbiguous)
	})
	t.Run("the store listing does not answer", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		sessions := filepath.Join(l.codexHome, "sessions")
		release := releasableLookupStall(t, readDirSeam, func(dir string) bool {
			return dir == sessions || strings.HasPrefix(dir, sessions+string(filepath.Separator))
		})
		withinBound(t, func() bool { snap.TrackCodexReceiptPool(true); return true })
		release()
		require.Empty(t, snap.receiptPaths, "precondition: a listing that did not answer tracks nothing")
		l.briefBoth(t)
		requireReceiptNeverConfirms(t, snap)
	})
}

// #7384 review R5. The config agent pins an explicit `codex resume <id>` by
// listing the store for id's rollout (TrackCodexReceiptTarget). A listing that
// does not answer is not "no such rollout", which would leave the wait
// positive-only and let a concurrent C confirm the receipt alone.
func TestCodexPromptReceiptNeverConfirmsWhenThePinnedTargetsListingDoesNotAnswer(t *testing.T) {
	t.Run("every lookup answers", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		require.True(t, snap.TrackCodexReceiptTarget(heldTestOwnID), "precondition: the id pins A")
		l.briefBoth(t)
		require.ErrorIs(t, receiptOnceWithinBound(t, snap, receiptRacePrompt).err, ErrPromptReceiptAmbiguous)
	})
	t.Run("the store listing does not answer", func(t *testing.T) {
		l := newReceiptRaceLayout(t)
		snap := l.snapshot()
		sessions := filepath.Join(l.codexHome, "sessions")
		release := releasableLookupStall(t, readDirSeam, func(dir string) bool {
			return dir == sessions || strings.HasPrefix(dir, sessions+string(filepath.Separator))
		})
		require.False(t, withinBound(t, func() bool { return snap.TrackCodexReceiptTarget(heldTestOwnID) }), "an id af could not look up is not pinned")
		release()
		l.briefBoth(t)
		requireReceiptNeverConfirms(t, snap)
	})
}

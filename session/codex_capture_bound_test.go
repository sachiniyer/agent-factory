package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
		received, undecided, err := codexPromptReceiptOnce(snap, prompt)
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

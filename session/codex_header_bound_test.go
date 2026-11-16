package session

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7370 review R2 and R3. A rollout's session_meta header is read inside
// fsprobe's bound (#7279), so a read that does not answer is a lookup that did
// not happen. It must be counted with the other lookups that did not answer:
// the restore resumes the recorded thread and retires nothing, the carry
// refuses, and capture waits. It is never a header that answered but is still
// being written (the restore's unverified fresh start). And it is never
// evidence that the record is stale, even when a later read of the same
// header answers.

// standInCodexRolloutOpen replaces the open behind every rollout header read
// with stand, which is handed the open it replaces, until the test ends.
func standInCodexRolloutOpen(t *testing.T, stand func(open func(string) (*os.File, error), path string) (*os.File, error)) {
	t.Helper()
	prev := codexRolloutOpenStandIn.Load()
	open := codexRolloutHeaderOpen()
	standIn := func(path string) (*os.File, error) { return stand(open, path) }
	codexRolloutOpenStandIn.Store(&standIn)
	t.Cleanup(func() { codexRolloutOpenStandIn.Store(prev) })
}

// stallCodexRolloutOpens makes every open of a rollout for which match is true
// block until the test ends, as a header on a wedged mount does, and bounds
// af's probes at 50ms. Every other rollout opens for real.
func stallCodexRolloutOpens(t *testing.T, match func(path string) bool) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	standInCodexRolloutOpen(t, func(open func(string) (*os.File, error), path string) (*os.File, error) {
		if match(path) {
			<-release
		}
		return open(path)
	})
	t.Cleanup(func() {
		close(release)
		// Let any flight still blocked in the open finish before the real
		// open comes back.
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
}

// rolloutNamed matches a rollout by its file name.
func rolloutNamed(name string) func(string) bool {
	return func(path string) bool { return filepath.Base(path) == name }
}

// firstCodexRolloutOpenFails makes the first open of the rollout at path fail
// as fail says, and every later one answer: the S2 sequence, where the read
// that dates a newer thread for an operator's repair does not answer and the
// read that correlates it a moment later does. fail runs the real open itself
// when it means to answer late. The launch dir's resolution, which the
// staleness judge runs between the two reads, waits for that first open to
// return, so its flight has unpublished (and its latch cleared) before the
// second read starts.
func firstCodexRolloutOpenFails(t *testing.T, path, launchDir string, fail func(open func(string) (*os.File, error), path string, release <-chan struct{}) (*os.File, error)) {
	t.Helper()
	var opens atomic.Int32
	release := make(chan struct{})
	returned := make(chan struct{})
	var once sync.Once
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	standInCodexRolloutOpen(t, func(open func(string) (*os.File, error), p string) (*os.File, error) {
		if p == path && opens.Add(1) == 1 {
			defer close(returned)
			return fail(open, p, release)
		}
		return open(p)
	})
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(p string) (string, error) {
		if p == filepath.Clean(launchDir) && opens.Load() >= 1 {
			once.Do(func() {
				close(release)
				<-returned
				// The flight unpublishes once its read returns, a few
				// statements after the open.
				time.Sleep(20 * time.Millisecond)
			})
		}
		return filepath.EvalSymlinks(p)
	}))
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
}

// answerLate is a first open that answers only after the read's bound.
func answerLate(open func(string) (*os.File, error), path string, release <-chan struct{}) (*os.File, error) {
	<-release
	return open(path)
}

// failWithEIO is a first open that fails at once, as a soft NFS mount or a
// reconnecting sshfs does.
func failWithEIO(_ func(string) (*os.File, error), path string, _ <-chan struct{}) (*os.File, error) {
	return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
}

// S2 (#7370 review R2), restore. An operator's repair overrode N, a thread of
// this worktree that was already in the store when the repair was made. The
// read that dates N (operatorRepairWindow.disregards) does not answer, so
// whether the repair overrode N is unknown. The read that correlates N a
// moment later answers and finds it this worktree's. That second answer must
// not make the repair stale: the verdict rests on a read that did not happen,
// so it is the no-evidence resume and retires nothing. A dating read that
// failed outright gets a failed read's answer, and is never stale either.
func TestOperatorRepairIsNotStaleWhenADatingReadDoesNotAnswerThenDoes(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	layout := func(t *testing.T) (inst *Instance, workDir, newer string) {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", home)
		workDir = filepath.Join(testguard.CanonicalTempDir(t), "wt")
		require.NoError(t, os.MkdirAll(workDir, 0o755))
		writeMintedCodexRollout(t, home, heldTestOwnID, workDir,
			now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), "cli")
		newer = writeMintedCodexRollout(t, home, repairNextID, workDir,
			repairedAt.Add(-5*time.Minute), repairedAt.Add(-5*time.Minute), now.Add(-time.Minute), "cli")
		inst = staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "precondition: a thread that existed at the repair never makes it stale: %+v", decline)
		return inst, workDir, newer
	}
	resume := func(t *testing.T, inst *Instance) exactResumeAnswer {
		return withinBound(t, func() exactResumeAnswer {
			program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
			return exactResumeAnswer{program, ok, decline}
		})
	}

	t.Run("the dating read does not answer, then the correlating read does", func(t *testing.T) {
		inst, workDir, newer := layout(t)
		firstCodexRolloutOpenFails(t, newer, workDir, answerLate)
		got := resume(t, inst)
		require.True(t, got.ok, "a read that did not answer resumes the repaired thread: %+v", got.decline)
		require.Contains(t, got.program, "resume "+heldTestOwnID)
		require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "the operator's repair is kept")
	})
	t.Run("the dating read fails, then the correlating read answers", func(t *testing.T) {
		inst, workDir, newer := layout(t)
		firstCodexRolloutOpenFails(t, newer, workDir, failWithEIO)
		got := resume(t, inst)
		require.NotEqual(t, staleCarryReason, got.decline.reason, "a thread af could not date is no evidence the repair is stale")
		require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "the operator's repair is kept")
	})
}

// S2 (#7370 review R2), carry. The carry runs the same staleness judge as the
// restore. A thread whose dating read did not answer refuses the carry as a
// look that did not happen (unverified). It never refuses it as stale, which
// would claim the user moved on from the repaired thread.
func TestCarryDoesNotCallAnOperatorRepairStaleWhenADatingReadDoesNotAnswerThenDoes(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	require.True(t, inst.SetAgentConversation(operatorRecordAt(carryTestID, repairedAt)))
	source := filepath.Join(home, ".codex")
	dir := inst.GetWorktreePath()
	recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID),
		codexSessionMetaLine(t, carryTestID, dir, now.Add(-2*time.Hour), "cli"))
	newer := writeCarryFile(t, source, codexRolloutRel(newerConversationID),
		codexSessionMetaLine(t, newerConversationID, dir, repairedAt.Add(-5*time.Minute), "cli"))
	setModTime(t, recorded, now.Add(-time.Hour))
	setModTime(t, newer, now.Add(-time.Minute))
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: a thread that existed at the repair does not refuse the carry")

	firstCodexRolloutOpenFails(t, newer, dir, answerLate)
	got := withinBound(t, inst.AccountSwapCarryRefusal)
	require.NotEqual(t, carryRefusalCode(staleCarryReason), got, "a thread af could not date is no evidence the repair is stale")
	require.Equal(t, carryRefusalCode(carryUncorrelatedReason), got, "the carry refuses as a look that did not happen")
}

// R3 (#7370 review), correlation. A newer rollout whose header read does not
// answer is uncorrelated, reported as unreadable (so capture waits on it), and
// counted unresolved: a lookup that did not happen, apart from a header that
// answered but is still being written.
func TestCodexRolloutCorrelationCountsAHeaderReadThatDoesNotAnswerAsUnresolved(t *testing.T) {
	dir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := writeCodexRolloutFileWithCwd(t, t.TempDir(), codexBoundRolloutName(codexBoundNewerID), dir)
	matches, _, _, _ := classifyCodexRolloutFiles([]string{path}, dir)
	require.Equal(t, []string{path}, matches, "precondition: the rollout is this launch dir's")

	stallCodexRolloutOpens(t, rolloutNamed(filepath.Base(path)))
	type answer struct {
		matches                  []string
		uncorrelated, unresolved int
		unreadable               error
	}
	got := withinBound(t, func() answer {
		m, u, r, e := classifyCodexRolloutFiles([]string{path}, dir)
		return answer{m, u, r, e}
	})
	require.Empty(t, got.matches)
	require.Equal(t, 1, got.uncorrelated)
	require.Equal(t, 1, got.unresolved, "a header read that did not answer is a lookup that did not happen")
	require.True(t, errors.Is(got.unreadable, fsprobe.ErrProbeTimedOut), "and a channel capture could not look through: %v", got.unreadable)
}

// R3 (#7370 review), restore. A header read the guard needed that does not
// answer gives the no-evidence resume, whoever needed it: the correlation of a
// newer rollout of a captured record, or the provenance of an operator's
// repair. Neither starts the restore fresh, and neither retires the record.
func TestCodexRestoreGuardResumesWhenAHeaderReadDoesNotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		newer   bool
		stalled string
	}{
		{"a captured record, the newer rollout's header", ConversationCaptureCodexRollout, true, codexBoundNewerID},
		{"an operator's repair, its own rollout's header", ConversationCaptureOperator, false, codexBoundRecordedID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := filepath.Join(testguard.CanonicalTempDir(t), "wt")
			require.NoError(t, os.MkdirAll(wt, 0o755))
			newerCwd := ""
			if tc.newer {
				newerCwd = wt
			}
			inst := codexRestoreGuardLayout(t, tc.kind, wt, wt, newerCwd)
			stallCodexRolloutOpens(t, rolloutNamed(codexBoundRolloutName(tc.stalled)))
			got := withinBound(t, func() exactResumeAnswer {
				program, ok, decline := prepareExactResumeConversation(inst, "codex")
				return exactResumeAnswer{program, ok, decline}
			})
			require.True(t, got.ok, "a header read that did not answer resumes the recorded thread: %+v", got.decline)
			require.Contains(t, got.program, "resume "+codexBoundRecordedID)
			require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired")
		})
	}
}

// R3 (#7370 review), carry. The carry's answer to the same read is a refusal:
// it never carries an operator's repair whose provenance it could not read.
func TestCodexCarryRefusesWhenTheRepairsHeaderReadDoesNotAnswer(t *testing.T) {
	now := time.Now()
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	require.True(t, inst.SetAgentConversation(operatorRecordAt(carryTestID, now.Add(-10*time.Minute))))
	recorded := writeCarryFile(t, filepath.Join(home, ".codex"), codexRolloutRel(carryTestID),
		codexSessionMetaLine(t, carryTestID, inst.GetWorktreePath(), now.Add(-2*time.Hour), "cli"))
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the repair carries")

	stallCodexRolloutOpens(t, rolloutNamed(filepath.Base(recorded)))
	require.NotEmpty(t, withinBound(t, inst.AccountSwapCarryRefusal), "a repair af could not read is not carried")
}

// set-conversation reads the rollout's header to validate the repair. A read
// that does not answer refuses the repair, and the refusal names the mount, as
// a directory that did not resolve does, not a header "missing or incomplete"
// (#7370 review R6).
func TestValidateSettableConversationNamesTheMountWhenTheHeaderReadDoesNotAnswer(t *testing.T) {
	setConversationConfig(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	name := "rollout-2026-07-06T10-17-35-" + setConversationID + ".jsonl"
	writeCodexRolloutFileWithCwd(t, codexHome, name, workDir)
	inst := setConversationInstance(t, workDir)
	_, err := ValidateSettableConversation(inst, setConversationID)
	require.NoError(t, err, "precondition: the repair validates")

	stallCodexRolloutOpens(t, rolloutNamed(name))
	err = withinBound(t, func() error {
		_, err := ValidateSettableConversation(inst, setConversationID)
		return err
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the refusal carries the bounded read's non-answer: %v", err)
	require.Contains(t, err.Error(), "its filesystem did not answer")
	require.Contains(t, err.Error(), "then retry")
	require.NotContains(t, err.Error(), "missing or incomplete")
}

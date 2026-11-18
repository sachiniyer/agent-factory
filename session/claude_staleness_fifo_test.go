package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// #7389 site 6 regression: claudeTranscriptExtends opens both transcript paths
// inside its paced flight, and a plain os.Open inside the flight wedges
// forever if a FIFO is swapped in at a path the listing had just verified as a
// regular file (or an open stalls on a dead mount). The work goroutine blocks
// in openat, the flight times out, and the registry entry latches: every
// later ask for the same pair is refused instantly with "still running after
// an earlier deadline", the session's staleness answer degrades permanently to
// unverified, and the goroutine leaks until the daemon restarts. The open is
// fsprobe.OpenRegular — O_NONBLOCK|O_NOFOLLOW with the shared
// *fsprobe.NonRegularError refusal on the opened descriptor, the same guard
// every bounded sibling carries (openCarryFileAt, openCodexRollout) — so a
// planted FIFO is answered within the bound, and answered as a look that did
// not happen: carryLookupUnlooked classes the typed refusal as unverified,
// never as evidence the copies diverge (#7527).

// fifoPatrolBound is the per-flight bound the tests here install. It is a
// ceiling, not a measurement: the correctness property is that each ask is
// answered without hitting it — a wedge is caught by the bound, or by the
// helpers' own ceilings behind it. Generous, so a loaded runner cannot flake
// it.
const fifoPatrolBound = 2 * time.Second

// drainBlockedOpenatGoroutine waits for the goroutine count to return to
// baseline, the way assertNoLeakedGoroutine does. A bounded open returns
// immediately, so the flight's work goroutine drains; a wedged openat (the
// bug) keeps it alive forever, so the count never settles.
func drainBlockedOpenatGoroutine(t *testing.T, baseline int) {
	t.Helper()
	runtime.GC()
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a goroutine leaked into a blocked openat did not drain (baseline=%d, now=%d)",
		baseline, runtime.NumGoroutine())
}

// requireClaudeTranscriptExtendsWithinBound calls claudeTranscriptExtends and
// fails the test if it does not return within a hard ceiling, the way a wedged
// open would hang the caller. The bound installed is far shorter, so a flight
// that times out still returns well within this ceiling.
func requireClaudeTranscriptExtendsWithinBound(t *testing.T, longer, shorter claudeTranscript) (bool, error) {
	t.Helper()
	type result struct {
		extends bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		ext, err := claudeTranscriptExtends(longer, shorter)
		done <- result{ext, err}
	}()
	select {
	case r := <-done:
		return r.extends, r.err
	case <-time.After(3 * time.Second):
		t.Fatal("claudeTranscriptExtends did not return — a planted FIFO wedged the paced flight's open")
		return false, nil
	}
}

// requireOpenClaudeTranscriptWithinBound is the same hang guard for the open
// helper itself.
func requireOpenClaudeTranscriptWithinBound(t *testing.T, p carryPace, path string) (*os.File, os.FileInfo, error) {
	t.Helper()
	type result struct {
		file *os.File
		info os.FileInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		f, i, err := openClaudeTranscript(p, path)
		done <- result{f, i, err}
	}()
	select {
	case r := <-done:
		return r.file, r.info, r.err
	case <-time.After(3 * time.Second):
		t.Fatal("openClaudeTranscript did not return — a planted FIFO wedged the bounded open")
		return nil, nil, nil
	}
}

// unblockFifoOnCleanup opens the FIFO for writing non-blocking on cleanup, so a
// reader that should not exist (the open is non-blocking now) cannot hang the
// test runner if a future regression reintroduces a blocking open.
func unblockFifoOnCleanup(t *testing.T, fifo string) {
	t.Helper()
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
			_ = f.Close()
		}
		_ = os.Remove(fifo)
	})
}

// TestOpenClaudeTranscript_RefusesAFIFOWithoutWedging plants a FIFO at a
// transcript path and proves the bounded open refuses it within the bound as a
// non-regular file, rather than blocking in openat.
func TestOpenClaudeTranscript_RefusesAFIFOWithoutWedging(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	fifo := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	unblockFifoOnCleanup(t, fifo)

	file, info, err := requireOpenClaudeTranscriptWithinBound(t, pacedCarryWalk(nil), fifo)
	require.Error(t, err, "a FIFO at the transcript path is refused")
	require.ErrorContains(t, err, "not a regular file", "the refusal names the non-regular file")
	var nonRegular *fsprobe.NonRegularError
	require.ErrorAs(t, err, &nonRegular, "the refusal carries the typed shape classifiers see")
	require.Nil(t, file, "no descriptor is kept on a non-regular refusal")
	require.Nil(t, info)
	require.False(t, errors.Is(err, fsprobe.ErrProbeTimedOut),
		"the FIFO must be answered within the bound, not by timing the flight out: %v", err)
}

// TestOpenClaudeTranscript_RefusesALeafSymlinkWithoutFollowing plants a symlink
// at a transcript path and proves O_NOFOLLOW refuses it, rather than following
// the symlink into another file's contents the way a plain os.Open would.
func TestOpenClaudeTranscript_RefusesALeafSymlinkWithoutFollowing(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	target := filepath.Join(dir, "real.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("{\"cwd\":\"/impostor\"}\n"), 0o600))
	link := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.Symlink(target, link))

	file, _, err := requireOpenClaudeTranscriptWithinBound(t, pacedCarryWalk(nil), link)
	require.Error(t, err, "O_NOFOLLOW refuses a symlink at the leaf")
	require.ErrorIs(t, err, syscall.ELOOP, "the open fails with ELOOP the way O_NOFOLLOW specifies")
	require.Nil(t, file, "the symlink must not be followed into the target")
}

// TestOpenClaudeTranscript_OpensARegularFile is the control: a regular
// transcript file opens and stats without error.
func TestOpenClaudeTranscript_OpensARegularFile(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"base\":1}\n"), 0o600))

	file, info, err := requireOpenClaudeTranscriptWithinBound(t, pacedCarryWalk(nil), path)
	require.NoError(t, err)
	require.NotNil(t, file)
	defer file.Close()
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, int64(len("{\"base\":1}\n")), info.Size())
}

// TestClaudeTranscriptExtends_AFIFOAtTheLongerPathDoesNotWedgTheFlight is the
// core regression: a FIFO swapped in at the longer transcript path after the
// listing must be answered within the bound, must not latch the flight's
// registry against the next ask, and must not leak a goroutine into a blocked
// openat.
func TestClaudeTranscriptExtends_AFIFOAtTheLongerPathDoesNotWedgTheFlight(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	longPath := filepath.Join(dir, "longer.jsonl")
	shortPath := filepath.Join(dir, "shorter.jsonl")
	require.NoError(t, os.WriteFile(shortPath, []byte("{\"base\":1}\n"), 0o600))
	require.NoError(t, syscall.Mkfifo(longPath, 0o600))
	unblockFifoOnCleanup(t, longPath)

	longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
	shorter := claudeTranscript{dir: dir, name: filepath.Base(shortPath)}

	baseline := runtime.NumGoroutine()

	// First call: the FIFO is answered within the bound, as a look that did
	// not happen — the guarded open refuses the non-regular file with
	// *fsprobe.NonRegularError, and unlookedOnly propagates it, so the verdict
	// is unverified, never that the copies diverge (#7527).
	extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.False(t, errors.Is(err, fsprobe.ErrProbeTimedOut),
		"a FIFO at the open path must not time the flight out (it is answered within the bound): %v", err)
	require.Error(t, err, "a non-regular file is a look that did not happen, not an answer")
	var nonRegular *fsprobe.NonRegularError
	require.ErrorAs(t, err, &nonRegular, "the refusal carries the typed shape classifiers see")
	require.True(t, carryLookupUnlooked(err),
		"the refusal is a look that did not happen — never evidence the copies diverge")
	require.False(t, extends)

	// The work goroutine returned — the registry entry is gone — so a second
	// ask for the same pair starts its own flight and answers the same way,
	// rather than being refused as "still running after an earlier deadline"
	// while a wedged goroutine keeps the entry latched.
	extends, err = requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.False(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the second ask is not refused by a latched flight: %v", err)
	require.Error(t, err)
	require.True(t, carryLookupUnlooked(err), "the second ask answers the same refusal, not a latched entry")
	require.NotContains(t, err.Error(), "still running after an earlier deadline",
		"the second ask is not refused by a latched flight")
	require.False(t, extends)

	// No goroutine leaked into a blocked openat.
	drainBlockedOpenatGoroutine(t, baseline)
}

// TestClaudeTranscriptExtends_AFIFOAtTheShorterPathDoesNotWedgTheFlight
// covers the same hazard on the second open. The fix guards both paths.
func TestClaudeTranscriptExtends_AFIFOAtTheShorterPathDoesNotWedgTheFlight(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	longPath := filepath.Join(dir, "longer.jsonl")
	shortPath := filepath.Join(dir, "shorter.jsonl")
	require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":1}\n{\"more\":1}\n"), 0o600))
	require.NoError(t, syscall.Mkfifo(shortPath, 0o600))
	unblockFifoOnCleanup(t, shortPath)

	longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
	shorter := claudeTranscript{dir: dir, name: filepath.Base(shortPath)}

	baseline := runtime.NumGoroutine()

	extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.False(t, errors.Is(err, fsprobe.ErrProbeTimedOut),
		"a FIFO at the shorter path must not time the flight out: %v", err)
	require.Error(t, err, "a non-regular file is a look that did not happen, not an answer")
	require.True(t, carryLookupUnlooked(err),
		"the refusal is a look that did not happen — never evidence about the copies")
	require.False(t, extends, "a FIFO at the shorter path does not extend anything")

	extends, err = requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.Error(t, err)
	require.True(t, carryLookupUnlooked(err), "the second ask answers the same refusal, not a latched entry")
	require.NotContains(t, err.Error(), "still running after an earlier deadline",
		"the second ask is not refused by a latched flight")
	require.False(t, extends)

	drainBlockedOpenatGoroutine(t, baseline)
}

// TestClaudeTranscriptExtends_RecoversAfterTheFIFOIsRemoved proves the fix
// recovers: once the FIFO is gone and a regular transcript is back at the
// path, the comparison answers normally. A wedged open would still be blocked
// in openat on the unlinked inode, so the next ask would still be refused.
func TestClaudeTranscriptExtends_RecoversAfterTheFIFOIsRemoved(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	longPath := filepath.Join(dir, "longer.jsonl")
	shortPath := filepath.Join(dir, "shorter.jsonl")
	require.NoError(t, os.WriteFile(shortPath, []byte("{\"base\":1}\n"), 0o600))
	require.NoError(t, syscall.Mkfifo(longPath, 0o600))
	unblockFifoOnCleanup(t, longPath)

	longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
	shorter := claudeTranscript{dir: dir, name: filepath.Base(shortPath)}

	// The FIFO is answered within the bound (no wedge), as a look that did
	// not happen.
	extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.False(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the FIFO did not wedge the flight: %v", err)
	require.Error(t, err, "a non-regular file is a look that did not happen, not an answer")
	require.True(t, carryLookupUnlooked(err))
	require.False(t, extends)

	// Replace the FIFO with a regular transcript that holds every byte of the
	// shorter copy plus a turn, and re-judge. A wedged open would still be
	// blocked and the next ask refused; the bounded open judges the new file.
	require.NoError(t, os.Remove(longPath))
	require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":1}\n{\"more\":1}\n"), 0o600))

	extends, err = requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.NoError(t, err)
	require.True(t, extends, "a longer copy that begins with every byte of the shorter one extends it")
}

// TestClaudeTranscriptExtends_RegularFilesStillCompare is the no-regression
// control: two regular transcripts compare the way they did before the open
// was guarded — one that extends reports true, and one that diverges false.
func TestClaudeTranscriptExtends_RegularFilesStillCompare(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	shortPath := filepath.Join(dir, "shorter.jsonl")
	require.NoError(t, os.WriteFile(shortPath, []byte("{\"base\":1}\n"), 0o600))
	shorter := claudeTranscript{dir: dir, name: filepath.Base(shortPath)}

	t.Run("a longer copy that begins with the shorter extends it", func(t *testing.T) {
		longPath := filepath.Join(dir, "extends.jsonl")
		require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":1}\n{\"more\":1}\n"), 0o600))
		longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
		extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
		require.NoError(t, err)
		require.True(t, extends)
	})

	t.Run("a longer copy that diverges does not extend it", func(t *testing.T) {
		longPath := filepath.Join(dir, "diverges.jsonl")
		require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":2}\n"), 0o600))
		longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
		extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
		require.NoError(t, err)
		require.False(t, extends)
	})

	t.Run("an identical file extends itself", func(t *testing.T) {
		longPath := filepath.Join(dir, "same.jsonl")
		require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":1}\n"), 0o600))
		longer := claudeTranscript{dir: dir, name: filepath.Base(longPath)}
		extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
		require.NoError(t, err)
		require.True(t, extends)
	})
}

// TestStatClaudeCopy_AFIFOAtTheCopyPathIsALookThatDidNotHappen is the carry
// side's end-to-end pin (#7527): a FIFO planted at the copy's path — regular
// to the listing that produced the plan, or planted by a writer between plan
// and lookup — opens non-regular, and the typed refusal must reach the copy
// note's own classification as a look that did not happen, so locateClaude
// refuses unverified. Before the refusal was typed it surfaced as a plain
// error that note swallowed: the copy read as absent, and a second spelling's
// copy could be judged "duplicated" instead.
func TestStatClaudeCopy_AFIFOAtTheCopyPathIsALookThatDidNotHappen(t *testing.T) {
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(fifoPatrolBound)
	t.Cleanup(restoreTimeout)

	configDir := t.TempDir()
	project := "projects/-repo"
	fifo := filepath.Join(configDir, project, carryTestID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(fifo), 0o700))
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	unblockFifoOnCleanup(t, fifo)

	copy, err := statClaudeCopy(ambientCarryRoot(configDir), project, carryTestID)
	require.Nil(t, copy, "a FIFO where the copy belongs is no copy")
	require.Error(t, err)
	var nonRegular *fsprobe.NonRegularError
	require.ErrorAs(t, err, &nonRegular,
		"the copy note's own classifier must see the typed refusal, not a plain error")
	require.True(t, carryLookupUnlooked(err),
		"the FIFO is a look that did not happen — locateClaude refuses unverified, never duplicated")
}

// TestClaudeTranscriptExtends_OpenAndStatEachHoldTheirOwnBound pins the
// #7527-review pacing property: the staleness flight's bound is per SYSCALL,
// so a responsive mount where the open answers inside the bound and the
// fstat answers inside the bound — together exceeding it — must still
// answer. OpenRegularPaced restarts the bound at the fstat step; keep the
// marks outside the shared open instead and both syscalls share the open's
// window, which times the flight out into an unverified verdict.
func TestClaudeTranscriptExtends_OpenAndStatEachHoldTheirOwnBound(t *testing.T) {
	clock := fsprobe.NewManualClock(time.Now())
	t.Cleanup(fsprobe.SetFlightClockForTest(clock))
	t.Cleanup(fsprobe.SetProbeTimeoutForTest(fifoPatrolBound))

	dir := t.TempDir()
	longPath := filepath.Join(dir, "longer.jsonl")
	shortPath := filepath.Join(dir, "shorter.jsonl")
	require.NoError(t, os.WriteFile(shortPath, []byte("{\"base\":1}\n"), 0o600))
	require.NoError(t, os.WriteFile(longPath, []byte("{\"base\":1}\n{}\n"), 0o600))
	longer := claudeTranscript{dir: dir, name: "longer.jsonl"}
	shorter := claudeTranscript{dir: dir, name: "shorter.jsonl"}

	// A responsive-but-slow mount: every open costs three quarters of the
	// bound at its mark, and every fstat the same at the syscall itself —
	// together one and a half bounds per transcript.
	stall := 3 * fifoPatrolBound / 4
	prev := carryLookupStandIn.Load()
	standIn := func(op, path string) {
		if op == "open" && (path == longPath || path == shortPath) {
			clock.Advance(stall)
		}
	}
	carryLookupStandIn.Store(&standIn)
	t.Cleanup(func() { carryLookupStandIn.Store(prev) })
	t.Cleanup(fsprobe.SetRegularFileStatForTest(func(f *os.File) (os.FileInfo, error) {
		if f.Name() == longPath || f.Name() == shortPath {
			clock.Advance(stall)
			// The timer fires inside Advance, but its channel is buffered —
			// the waiter must be scheduled to read it before this syscall
			// returns and the next step refreshes lastStep. Hold the stat a
			// moment in real time: a flight whose fstat outlived the bound
			// still in progress is the timeout case being discriminated.
			time.Sleep(100 * time.Millisecond)
		}
		return f.Stat()
	}))

	extends, err := requireClaudeTranscriptExtendsWithinBound(t, longer, shorter)
	require.NoError(t, err,
		"open and fstat each answered inside their own bound window — the flight must answer, not time out: %v", err)
	require.True(t, extends, "the longer transcript still extends the shorter")
}

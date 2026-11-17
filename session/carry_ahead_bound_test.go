package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7383: the handoff engine asks require_carry's check-ahead
// (AccountSwapCarryRefusal, AccountSwapCarryRefusalTo) on its decision tick.
// Its reads in an account's home go through the carry's descriptor walk (an
// openat chain from an anchored root, then fstat and prefix reads), which
// fsprobe's path probes cannot reach. On a mount that stopped answering each
// one held the tick forever. Every one now runs inside a bounded flight, and
// a lookup that does not answer refuses the carry as a look that did not
// happen (unverified): it never carries, and it never hangs.

// carryLookBound is the per-syscall bound the tests here install: af's 2s
// bound shrunk so a lookup that does not answer costs 50ms, not seconds.
const carryLookBound = 50 * time.Millisecond

// stallCarryLookups makes every descriptor-walk syscall match accepts block
// until the test ends, as one on a wedged mount does, and bounds af's probes
// at carryLookBound.
func stallCarryLookups(t *testing.T, match func(op, path string) bool) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(carryLookBound)
	prev := carryLookupStandIn.Load()
	standIn := func(op, path string) {
		if match(op, path) {
			<-release
		}
	}
	carryLookupStandIn.Store(&standIn)
	t.Cleanup(func() {
		close(release)
		// Let any walk still blocked in the stand-in finish before the real
		// syscalls come back.
		time.Sleep(50 * time.Millisecond)
		carryLookupStandIn.Store(prev)
		restoreTimeout()
	})
}

func carryOpOn(op, path string) func(string, string) bool {
	return func(o, p string) bool { return o == op && p == path }
}

// codexAccountCarry is a Codex session that ran under account "old", whose
// recorded conversation the check-ahead judges, swapping to "work".
func codexAccountCarry(t *testing.T) (inst *Instance, source, target string) {
	t.Helper()
	useCarryAheadStores(t)
	carryAmbientProviderHomes(t)
	inst, target = carrySwapInstance(t, tmux.ProgramCodex)
	source = registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	return inst, source, target
}

// Site 1: a Codex thread with no plain rollout in the source store is missing,
// once the carry has probed the store's sessions directory to name a symlink
// there instead. A probe that does not answer is no "missing": the check-ahead
// refuses as unverified.
func TestCodexCarryCheckAheadRefusesWhenTheStoreProbeDoesNotAnswer(t *testing.T) {
	inst, source, _ := codexAccountCarry(t)
	require.NoError(t, os.MkdirAll(filepath.Join(source, "sessions"), 0o700))
	require.Equal(t, handoff.CarryMissing, inst.AccountSwapCarryRefusal(), "precondition: a thread with no rollout is missing")

	stallCarryLookups(t, carryOpOn("openat", filepath.Join(source, "sessions")))
	require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
}

// Site 2: against a target that already holds an older copy of the rollout,
// the check-ahead judges the copy as the copy itself would (judgeCarryFile):
// it opens both copies and compares the target's against the source's prefix.
// An open or a read that does not answer refuses as unverified.
func TestCarryCheckAheadRefusesWhenTheTargetCopyJudgementDoesNotAnswer(t *testing.T) {
	layout := func(t *testing.T) (inst *Instance, src, dst string) {
		inst, source, target := codexAccountCarry(t)
		full := codexRollout(inst.GetWorktreePath())
		src = writeCarryFile(t, source, codexRolloutRel(carryTestID), full)
		dst = writeCarryFile(t, target, codexRolloutRel(carryTestID), strings.SplitAfter(full, "\n")[0])
		require.Empty(t, inst.AccountSwapCarryRefusalTo("work"), "precondition: an older target copy carries")
		return inst, src, dst
	}
	for _, tc := range []struct {
		name  string
		stall func(src, dst string) func(string, string) bool
	}{
		{"the source copy's open", func(src, _ string) func(string, string) bool { return carryOpOn("openat", src) }},
		{"the target copy's read", func(_, dst string) func(string, string) bool { return carryOpOn("read", dst) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, src, dst := layout(t)
			stallCarryLookups(t, tc.stall(src, dst))
			require.Equal(t, handoff.CarryUnverified, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }))
		})
	}
}

// Site 3: a Claude carry finds the recorded conversation's copy under each
// spelling of the worktree's path. A copy whose lookup does not answer may be
// the one Claude is appending to, so the check-ahead refuses as unverified,
// rather than carry another or call it missing.
func TestClaudeCarryCheckAheadRefusesWhenACopyLookupDoesNotAnswer(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	copyPath := writeCarryFile(t, filepath.Join(home, ".claude"), claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the copy carries")

	stallCarryLookups(t, carryOpOn("openat", copyPath))
	require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
}

// claudeTwoSpellingCarry is a Claude conversation filed under both spellings
// of a symlinked worktree's path, the recorded spelling's copy holding
// recorded and the resolved spelling's holding resolved, the resolved one
// written a minute later.
func claudeTwoSpellingCarry(t *testing.T, recordedBody, resolvedBody string) (inst *Instance, recordedPath, resolvedPath string) {
	t.Helper()
	home := carryAmbientProviderHomes(t)
	inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	source := filepath.Join(home, ".claude")
	past := time.Now().Add(-time.Hour)
	recordedPath = writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), recordedBody)
	resolvedPath = writeCarryFile(t, source, filepath.Join(resolved, carryTestID+".jsonl"), resolvedBody)
	setModTime(t, recordedPath, past)
	setModTime(t, resolvedPath, past.Add(time.Minute))
	return inst, recordedPath, resolvedPath
}

// Site 4: with two copies, the carry takes the one that holds every byte of
// the other, which it learns by comparing their prefixes. A read that does not
// answer tells af neither that one extends the other nor that they diverge, so
// the check-ahead refuses as unverified rather than claim they diverge.
func TestClaudeCarryCheckAheadRefusesWhenACopyComparisonDoesNotAnswer(t *testing.T) {
	inst, _, resolvedPath := claudeTwoSpellingCarry(t, "{\"base\":1}\n", "{\"base\":1}\n{\"more\":1}\n")
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the copy that extends the other carries")

	stallCarryLookups(t, carryOpOn("read", resolvedPath))
	require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
}

// Site 5: against a target, a Claude carry looks for a copy the new account
// already files under another spelling, which its Claude could resume instead
// of the carried one. A lookup there that does not answer refuses as
// unverified: af could not check the new account's home.
func TestClaudeCarryCheckAheadRefusesWhenTheTargetsOtherCopyLookupDoesNotAnswer(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
	target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	writeCarryFile(t, filepath.Join(home, ".claude"), filepath.Join(recorded, carryTestID+".jsonl"), "{}\n{}\n")
	other := writeCarryFile(t, target, filepath.Join(resolved, carryTestID+".jsonl"), "{\"stale\":true}\n")
	require.Equal(t, handoff.CarryDuplicated, inst.AccountSwapCarryRefusalTo("work"),
		"precondition: the other copy refuses the carry against that target")

	stallCarryLookups(t, carryOpOn("openat", other))
	require.Equal(t, handoff.CarryUnverified, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }))
}

// Site 6: the carry's staleness check judges the bound copy against a newer
// copy of the conversation under another spelling, by whether the bound copy
// still holds every byte of it. A comparison that does not answer refuses the
// carry as unverified, never as copies that diverge. (The restore judges with
// no bound copy, taking the newest, so no copy is newer and it never compares.)
func TestClaudeCarryCheckAheadRefusesWhenTheStalenessComparisonDoesNotAnswer(t *testing.T) {
	// The recorded copy holds the resolved one, which was touched later: the
	// carry binds the recorded copy, and judges the resolved one newer.
	inst, _, resolvedPath := claudeTwoSpellingCarry(t, "{\"base\":1}\n{\"more\":1}\n", "{\"base\":1}\n")
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: a newer copy the bound one holds refuses nothing")

	stallCarryLookups(t, carryOpOn("open", resolvedPath))
	require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
}

// paceCarryLookups makes every descriptor-walk syscall answer after delay on
// a manual bound clock it installs, as on a slow mount that still answers
// (#7456): a syscall's cost is declared on the clock, never slept, so no
// scheduler can stretch one read past the bound. When n is positive, the
// n-th read of stallPath spends a whole bound on the clock and then does not
// answer at all until the test ends — the flight has timed out on it before
// the walk resumes.
func paceCarryLookups(t *testing.T, delay time.Duration, stallPath string, n int32) {
	t.Helper()
	release := make(chan struct{})
	clock := fsprobe.NewManualClock(time.Now())
	restoreClock := fsprobe.SetPacedClockForTest(clock)
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(carryLookBound)
	prev := carryLookupStandIn.Load()
	var reads atomic.Int32
	standIn := func(op, path string) {
		if n > 0 && op == "read" && path == stallPath && reads.Add(1) == n {
			clock.Advance(carryLookBound)
			<-release
			return
		}
		clock.Advance(delay)
	}
	carryLookupStandIn.Store(&standIn)
	t.Cleanup(func() {
		close(release)
		// Let any walk still blocked in the stand-in finish before the real
		// syscalls come back.
		time.Sleep(50 * time.Millisecond)
		carryLookupStandIn.Store(prev)
		restoreTimeout()
		restoreClock()
	})
}

// slowRead is how long each walk syscall takes on the slow mount below: one
// twenty-fifth of the bound.
const slowRead = carryLookBound / 25

// largeCopyBody is a transcript of about 4 MiB after first: 64 of the
// compare's 64 KiB chunks per file, so a compare of two copies makes 128
// reads, which at slowRead apiece is five bounds in all.
func largeCopyBody(first string) string {
	line := `{"type":"response_item","payload":{"text":"` + strings.Repeat("x", 4000) + `"}}` + "\n"
	return first + strings.Repeat(line, 1024)
}

// #7389 review R1. The bound is per read, never per judgement: a compare of
// two large copies on a slow mount whose every read answers within the bound
// reaches the answer every lookup answering gives, however many bounds its
// reads take in all, in the check-ahead and in the live plan. A single read
// that does not answer still refuses, as unverified.
func TestCodexCarryJudgesALargeTargetCopyOnASlowMountWhoseReadsAnswer(t *testing.T) {
	layout := func(t *testing.T) (*Instance, string) {
		inst, source, target := codexAccountCarry(t)
		full := largeCopyBody(codexRollout(inst.GetWorktreePath()))
		writeCarryFile(t, source, codexRolloutRel(carryTestID), full)
		// The account holds the copy from an earlier swap, one turn short.
		older := full[:strings.LastIndex(strings.TrimSuffix(full, "\n"), "\n")+1]
		dst := writeCarryFile(t, target, codexRolloutRel(carryTestID), older)
		return inst, dst
	}
	t.Run("every read answers", func(t *testing.T) {
		inst, _ := layout(t)
		paceCarryLookups(t, slowRead, "", 0)
		require.Empty(t, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }), "the check-ahead carries")
		require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
		require.NotNil(t, inst.accountSwapLaunch.carry, "the live plan carries: %q", inst.accountSwapLaunch.carryFallback)
	})
	t.Run("one read does not answer", func(t *testing.T) {
		inst, dst := layout(t)
		paceCarryLookups(t, slowRead, dst, 10)
		require.Equal(t, handoff.CarryUnverified, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }))
	})
	t.Run("one read does not answer, in the live plan", func(t *testing.T) {
		inst, dst := layout(t)
		paceCarryLookups(t, slowRead, dst, 10)
		requireCarryRefusedUnverified(t, inst)
	})
}

// #7389 review R1, the Claude copy comparison: two large copies under two
// spellings, the newer extending the older, on a slow mount whose reads
// answer. The carry takes the newer, as it would with every read fast.
func TestClaudeCarryComparesLargeCopiesOnASlowMountWhoseReadsAnswer(t *testing.T) {
	layout := func(t *testing.T) (*Instance, string) {
		body := largeCopyBody("{\"base\":1}\n")
		inst, _, resolvedPath := claudeTwoSpellingCarry(t, body, body+"{\"more\":1}\n")
		return inst, resolvedPath
	}
	t.Run("every read answers", func(t *testing.T) {
		inst, resolvedPath := layout(t)
		paceCarryLookups(t, slowRead, "", 0)
		require.Empty(t, withinBound(t, inst.AccountSwapCarryRefusal), "the check-ahead carries")
		require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
		require.NotNil(t, inst.accountSwapLaunch.carry, "the live plan carries: %q", inst.accountSwapLaunch.carryFallback)
		require.Equal(t, filepath.Base(filepath.Dir(resolvedPath)), filepath.Base(filepath.Dir(inst.accountSwapLaunch.carry.transcript)),
			"it takes the copy that extends the other")
	})
	t.Run("one read does not answer", func(t *testing.T) {
		inst, resolvedPath := layout(t)
		paceCarryLookups(t, slowRead, resolvedPath, 10)
		require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
	})
}

// #7389 review R1, against a target: the new account holds an older copy
// under the other spelling, which the carried copy extends, so the carry goes
// ahead past it. The comparison runs inside the target lookup's own flight,
// paced by the same walk, so a large one on a slow mount still carries.
func TestClaudeCarryPastALargeOlderTargetCopyOnASlowMountWhoseReadsAnswer(t *testing.T) {
	layout := func(t *testing.T) (*Instance, string) {
		home := carryAmbientProviderHomes(t)
		inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
		target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		past := time.Now().Add(-time.Hour)
		body := largeCopyBody("{\"base\":1}\n")
		setModTime(t, writeCarryFile(t, filepath.Join(home, ".claude"), filepath.Join(resolved, carryTestID+".jsonl"),
			body+"{\"live\":1}\n"), past.Add(time.Minute))
		left := writeCarryFile(t, target, filepath.Join(recorded, carryTestID+".jsonl"), body)
		setModTime(t, left, past)
		return inst, left
	}
	t.Run("every read answers", func(t *testing.T) {
		inst, _ := layout(t)
		paceCarryLookups(t, slowRead, "", 0)
		require.Empty(t, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }), "the check-ahead carries")
		require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
		require.NotNil(t, inst.accountSwapLaunch.carry, "the live plan carries: %q", inst.accountSwapLaunch.carryFallback)
	})
	t.Run("one read does not answer", func(t *testing.T) {
		inst, left := layout(t)
		paceCarryLookups(t, slowRead, left, 10)
		require.Equal(t, handoff.CarryUnverified, withinBound(t, func() string { return inst.AccountSwapCarryRefusalTo("work") }))
	})
}

// #7389 review R1, the staleness comparison: a large bound copy and a newer
// copy under another spelling that it holds, on a slow mount whose reads
// answer. The carry goes ahead, as it would with every read fast.
func TestClaudeCarryStalenessComparesLargeCopiesOnASlowMountWhoseReadsAnswer(t *testing.T) {
	layout := func(t *testing.T) (*Instance, string) {
		body := largeCopyBody("{\"base\":1}\n")
		inst, _, resolvedPath := claudeTwoSpellingCarry(t, body+"{\"more\":1}\n", body)
		return inst, resolvedPath
	}
	t.Run("every read answers", func(t *testing.T) {
		inst, _ := layout(t)
		paceCarryLookups(t, slowRead, "", 0)
		require.Empty(t, withinBound(t, inst.AccountSwapCarryRefusal), "the check-ahead carries")
		require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
		require.NotNil(t, inst.accountSwapLaunch.carry, "the live plan carries: %q", inst.accountSwapLaunch.carryFallback)
	})
	t.Run("one read does not answer", func(t *testing.T) {
		inst, resolvedPath := layout(t)
		// The locate step compares the copies first; stall the staleness
		// comparison's own read, which opens the file by path.
		paceCarryLookups(t, slowRead, resolvedPath, 64+10)
		require.Equal(t, handoff.CarryUnverified, withinBound(t, inst.AccountSwapCarryRefusal))
	})
}

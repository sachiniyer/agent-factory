package session

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7286, from the round-2 independent review of #7278. A look that did not
// happen is never a reason to carry a stale transcript, start fresh, or retire
// the record, and a lookup that fails is as much a look that did not happen
// as one that stalls: only absence (ENOENT, ENOTDIR) is an answer.

// worktreeLookupFault makes the k-th lookup of cwd fault, counted from when it
// is installed. "stall" blocks it until the test ends (the bound is 50ms).
// "eio" fails it with EIO, and when it is the leaf of an ancestor walk, the
// walk's next lookup (cwd's parent, the symlinked one) fails with EIO too: a
// soft NFS mount or a reconnecting sshfs erroring for the length of one walk.
func worktreeLookupFault(t *testing.T, cwd string, k int32, mode string) *atomic.Int32 {
	t.Helper()
	wait := launchDirWedge(t, cwd)
	var seen atomic.Int32
	var armed atomic.Bool
	parent := filepath.Dir(cwd)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == cwd {
			armed.Store(false)
			if seen.Add(1) == k {
				if mode == "stall" {
					wait(path)
				} else {
					armed.Store(true)
					return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
				}
			}
		}
		if path == parent && armed.Swap(false) {
			return "", &fs.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
		}
		return filepath.EvalSymlinks(path)
	}))
	return &seen
}

// worktreeLookups counts run's lookups of cwd.
func worktreeLookups(t *testing.T, cwd string, run func()) int32 {
	t.Helper()
	var n atomic.Int32
	restore := fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == cwd {
			n.Add(1)
		}
		return filepath.EvalSymlinks(path)
	})
	run()
	restore()
	return n.Load()
}

// Item 1. A newer conversation of this worktree refuses the carry when every
// lookup answers. Whichever single walk of the worktree fails with EIO, the
// carry still refuses (stale, or unverified as a look that did not happen) and
// never carries: a fallback past EIO is not an answer, so it is never "another
// directory's conversation".
func TestClaudeCarryRefusesWhenOneWalkOfTheWorktreeFails(t *testing.T) {
	setup := func(t *testing.T) (*Instance, string) {
		t.Helper()
		home := carryAmbientProviderHomes(t)
		inst, recorded, _ := symlinkedClaudeCarryInstance(t)
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		source := filepath.Join(home, ".claude")
		cwd := inst.GetWorktreePath()
		past := time.Now().Add(-time.Hour)
		setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, cwd)), past)
		setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, newerConversationID+".jsonl"), claudeRecordLine(t, cwd)), past.Add(time.Minute))
		require.Equal(t, carryRefusalCode(staleCarryReason), inst.AccountSwapCarryRefusal(),
			"precondition: with every lookup answering, the newer conversation refuses the carry")
		return inst, cwd
	}
	refusals := []string{handoff.CarryStale, handoff.CarryUnverified}
	inst, cwd := setup(t)
	lookups := worktreeLookups(t, cwd, func() { _ = inst.AccountSwapCarryRefusal() })
	require.Positive(t, lookups)

	for k := int32(1); k <= lookups; k++ {
		t.Run(fmt.Sprintf("lookup %d of %d fails", k, lookups), func(t *testing.T) {
			inst, cwd := setup(t)
			seen := worktreeLookupFault(t, cwd, k, "eio")
			refusal := withinBound(t, inst.AccountSwapCarryRefusal)
			require.GreaterOrEqual(t, seen.Load(), k, "precondition: the check-ahead reached the lookup that fails")
			require.Contains(t, refusals, refusal, "the check-ahead refuses what a resolved answer refuses")
			require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
			if carry := inst.accountSwapLaunch.carry; carry != nil {
				t.Fatalf("a lookup that failed carried %s past a newer conversation", carry.transcript)
			}
			require.Contains(t, refusals, carryRefusalCode(inst.accountSwapLaunch.carryFallback),
				"the swap states why it starts fresh: %q", inst.accountSwapLaunch.carryFallback)
		})
	}
}

// Item 2. The recorded conversation's newest copy is under the resolved
// spelling, so the restore resumes it, though a newer conversation of this
// worktree is filed under the recorded spelling beside an older copy. A
// listing that missed the resolved spelling, its lookup stalled or failed,
// sees only the older copy and the newer conversation. It must not read that
// as Stale and start fresh, retiring the record: an incomplete listing is at
// most uncorrelated, and the restore resumes.
func TestClaudeRestoreNeverAbandonsOnAnIncompleteListing(t *testing.T) {
	setup := func(t *testing.T) (*Instance, string) {
		t.Helper()
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		workDir, recordedProject, resolvedProject := claudeRestoreWorktree(t)
		resolvedDir, err := filepath.EvalSymlinks(workDir)
		require.NoError(t, err)
		now := time.Now()
		line := claudeRecordLine(t, resolvedDir)
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, carryTestID+".jsonl"), line), now.Add(-2*time.Hour))
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, newerConversationID+".jsonl"), line), now.Add(-time.Hour))
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(resolvedProject, carryTestID+".jsonl"), line+line), now.Add(-30*time.Minute))
		return claudeRestoreInstance(t, workDir, carryTestID), workDir
	}
	restore := func(t *testing.T, inst *Instance) exactResumeAnswer {
		return withinBound(t, func() exactResumeAnswer {
			program, ok, decline := prepareExactResumeConversation(inst, "claude")
			return exactResumeAnswer{program, ok, decline}
		})
	}
	inst, cwd := setup(t)
	got := restore(t, inst)
	require.True(t, got.ok, "precondition: with every lookup answering, the newest copy is current and resumes: %+v", got.decline)
	lookups := worktreeLookups(t, cwd, func() { restore(t, inst) })
	require.Positive(t, lookups)

	for _, mode := range []string{"stall", "eio"} {
		for k := int32(1); k <= lookups; k++ {
			t.Run(fmt.Sprintf("%s of lookup %d of %d", mode, k, lookups), func(t *testing.T) {
				inst, cwd := setup(t)
				seen := worktreeLookupFault(t, cwd, k, mode)
				got := restore(t, inst)
				require.GreaterOrEqual(t, seen.Load(), k, "precondition: the restore reached the faulted lookup")
				require.True(t, got.ok, "a look that did not happen resumes the recorded id: %+v", got.decline)
				require.Equal(t, "claude --resume "+carryTestID, got.program)
				require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired")
			})
		}
	}
}

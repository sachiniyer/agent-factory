package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7279 (continuation of #7272): the Codex restore guard, the Codex carry,
// operator-repair provenance, the rollout-cwd correlation they share with
// capture, and the agent swap still read the launch dir unbounded. On a wedged
// mount (NFS, FUSE, a dead sshfs) each held its caller forever. Every one is
// now bounded, and one that does not answer within the bound is "could not
// look", mapped to each caller's own safe outcome.

const (
	codexBoundRecordedID = "019a0000-0000-7000-8000-000000000042"
	codexBoundNewerID    = "019a0000-0000-7000-8000-000000000043"
)

// codexBoundRolloutName is a rollout file name Codex would give id.
func codexBoundRolloutName(id string) string {
	return "rollout-2026-11-12T10-17-35-" + id + ".jsonl"
}

// codexRestoreGuardLayout is a Codex session in workDir, with its recorded
// rollout and, unless newerCwd is "", a newer rollout recording newerCwd.
func codexRestoreGuardLayout(t *testing.T, workDir, recordedCwd, newerCwd string) *Instance {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCodexRolloutFileWithCwd(t, home, codexBoundRolloutName(codexBoundRecordedID), recordedCwd), past)
	if newerCwd != "" {
		setModTime(t, writeCodexRolloutFileWithCwd(t, home, codexBoundRolloutName(codexBoundNewerID), newerCwd), past.Add(time.Minute))
	}
	gw, err := sessiongit.NewGitWorktreeFromStorage(t.TempDir(), workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title: "t", Program: tmux.ProgramCodex, gitWorktree: gw,
		Tabs: []*Tab{{Conversation: AgentConversationData{
			Agent: tmux.ProgramCodex, ID: codexBoundRecordedID, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout,
		}}},
	}
	inst.Path = gw.GetRepoPath()
	return inst
}

// stallLookupUnder holds the k-th symlink resolution of root or a path under
// it until the test ends, keeping its latch, as on a mount where that one
// lookup is lost; k == 0 only counts. It returns the count.
func stallLookupUnder(t *testing.T, root string, k int32) *atomic.Int32 {
	t.Helper()
	wait := launchDirWedge(t, root)
	var seen atomic.Int32
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			if seen.Add(1) == k {
				wait(path)
			}
		}
		return filepath.EvalSymlinks(path)
	}))
	return &seen
}

// The restore guard looks up the worktree for the attribution rule, then the
// launch dir, then each newer rollout's cwd. Any of those not answering within
// its bound attributes nothing: the no-evidence resume the attribution rule's
// own timeout gives. It is never the unverified decline, which would start the
// restore fresh, and never a decline that retires the record. Only a look that
// answered declines (#7287 review R1). The review's two cases are the
// unsymlinked worktree whose guard lookup latches the key the rollout's cwd
// shares (lookup 3), and a newer rollout from a sibling directory (lookup 4).
func TestCodexRestoreGuardResumesWhenAnyLookupDoesNotAnswer(t *testing.T) {
	type layout func(t *testing.T) (inst *Instance, base string)
	plain := func(t *testing.T) (string, string) {
		base := testguard.CanonicalTempDir(t)
		wt := filepath.Join(base, "wt")
		require.NoError(t, os.MkdirAll(wt, 0o755))
		return base, wt
	}
	linked := func(t *testing.T) (string, string, string) {
		base := testguard.CanonicalTempDir(t)
		real := filepath.Join(base, "real", "repo")
		require.NoError(t, os.MkdirAll(real, 0o755))
		require.NoError(t, os.Symlink(filepath.Dir(real), filepath.Join(base, "link")))
		return base, filepath.Join(base, "link", "repo"), real
	}
	sibling := func(t *testing.T, base string) string {
		other := filepath.Join(base, "other")
		require.NoError(t, os.MkdirAll(other, 0o755))
		return other
	}
	for _, tc := range []struct {
		name   string
		build  layout
		stales bool // the answer with every lookup answering: declined as stale
	}{
		{"unsymlinked, newer rollout in this worktree", func(t *testing.T) (*Instance, string) {
			base, wt := plain(t)
			return codexRestoreGuardLayout(t, wt, wt, wt), base
		}, true},
		{"unsymlinked, newer rollout from a sibling dir", func(t *testing.T) (*Instance, string) {
			base, wt := plain(t)
			return codexRestoreGuardLayout(t, wt, wt, sibling(t, base)), base
		}, false},
		{"symlinked, newer rollout in this worktree (kernel spelling)", func(t *testing.T) (*Instance, string) {
			base, wt, real := linked(t)
			return codexRestoreGuardLayout(t, wt, real, real), base
		}, true},
		{"symlinked, newer rollout from a sibling dir", func(t *testing.T) (*Instance, string) {
			base, wt, real := linked(t)
			return codexRestoreGuardLayout(t, wt, real, sibling(t, base)), base
		}, false},
		{"nothing newer", func(t *testing.T) (*Instance, string) {
			base, wt := plain(t)
			return codexRestoreGuardLayout(t, wt, wt, ""), base
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups int32
			t.Run("every lookup answers", func(t *testing.T) {
				inst, base := tc.build(t)
				seen := stallLookupUnder(t, base, 0)
				_, ok, decline := prepareExactResumeConversation(inst, "codex")
				require.Equal(t, !tc.stales, ok, "precondition: the resolved answer: %+v", decline)
				lookups = seen.Load()
			})
			require.Positive(t, lookups)
			for k := int32(1); k <= lookups; k++ {
				t.Run(fmt.Sprintf("lookup %d of %d does not answer", k, lookups), func(t *testing.T) {
					inst, base := tc.build(t)
					seen := stallLookupUnder(t, base, k)
					got := withinBound(t, func() exactResumeAnswer {
						program, ok, decline := prepareExactResumeConversation(inst, "codex")
						return exactResumeAnswer{program, ok, decline}
					})
					require.GreaterOrEqual(t, seen.Load(), k, "precondition: the guard reached the lookup that does not answer")
					require.True(t, got.ok, "a look that did not answer resumes the recorded thread: %+v", got.decline)
					require.Contains(t, got.program, "resume "+codexBoundRecordedID)
					require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired")
				})
			}
		})
	}
}

// codexCarryLayout is a Codex carry from account "old" whose recorded rollout
// was launched in the worktree, with a newer rollout of this worktree when
// newer is set.
func codexCarryLayout(t *testing.T, newer bool) *Instance {
	t.Helper()
	carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath())), past)
	if newer {
		setModTime(t, writeCarryFile(t, source, codexRolloutRel(newerConversationID), codexRollout(inst.GetWorktreePath())), past.Add(time.Minute))
	}
	return inst
}

// A Codex account-swap carry resolves the launch dir to correlate a newer
// rollout. One that does not answer within the bound refuses the carry: it
// never carries a rollout that may be stale.
func TestCodexCarryRefusesWhenTheLaunchDirDoesNotResolve(t *testing.T) {
	inst := codexCarryLayout(t, true)
	require.Equal(t, carryRefusalCode(staleCarryReason), inst.AccountSwapCarryRefusal(),
		"precondition: with the launch dir answering, the newer rollout refuses the carry")

	wedgeLaunchDirResolution(t, inst.GetWorktreePath())
	require.Equal(t, carryRefusalCode(carryUncorrelatedReason), withinBound(t, inst.AccountSwapCarryRefusal),
		"require_carry's check-ahead defers the move")
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	if carry := inst.accountSwapLaunch.carry; carry != nil {
		t.Fatalf("a look that did not happen carried %s", carry.transcript)
	}
	require.Equal(t, carryUncorrelatedReason, inst.accountSwapLaunch.carryFallback, "the swap states why it starts fresh")
}

// With nothing newer than the carried rollout there is nothing to correlate,
// so the carry never asks the launch dir, and a wedged one does not refuse it
// (#7287 review R3).
func TestCodexCarryWithNothingNewerCarriesOnAWedgedLaunchDir(t *testing.T) {
	inst := codexCarryLayout(t, false)
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: the rollout carries")

	wedgeLaunchDirResolution(t, inst.GetWorktreePath())
	require.Empty(t, withinBound(t, inst.AccountSwapCarryRefusal), "the check-ahead does not refuse on a lookup it never needed")
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	require.NotNil(t, inst.accountSwapLaunch.carry, "the live swap carries: %q", inst.accountSwapLaunch.carryFallback)
}

// Operator-repair provenance compares the directory the repaired rollout
// records with the launch dir. Either not answering within the bound is a look
// that did not happen: the unverified decline, which retires nothing, never
// "started in another directory", which would retire the operator's repair.
func TestOperatorRepairProvenanceDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	dir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := writeCodexRolloutFileWithCwd(t, t.TempDir(), codexBoundRolloutName(codexBoundRecordedID), dir)
	require.Equal(t, resumeDecline{}, operatorRepairProvenance(path, dir), "precondition: the repair is this session's")

	wedgeLaunchDirResolution(t, dir)
	got := withinBound(t, func() resumeDecline { return operatorRepairProvenance(path, dir) })
	require.True(t, got.unverified, "a look that did not happen is unverified: %+v", got)
	require.Equal(t, operatorRepairUnresolvedReason, got.reason)
	require.False(t, got.retiresRecord(AgentConversationData{Agent: tmux.ProgramCodex, ID: codexBoundRecordedID, CaptureKind: ConversationCaptureOperator}),
		"the operator's repair is kept for the next look")
}

// The restore guard, the carry and capture correlate each newer rollout by the
// cwd it records, usually the launch dir. A resolution of it that does not
// answer within the bound is a channel af could not look through: the rollout
// is uncorrelated and its error reported, so capture waits and the guard and
// carry give their could-not-look answers, never "another directory's".
func TestCodexRolloutCorrelationDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	dir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := writeCodexRolloutFileWithCwd(t, t.TempDir(), codexBoundRolloutName(codexBoundNewerID), dir)
	workingDir := pathutil.ResolveForCompare(dir)
	matches, uncorrelated, _, _ := classifyCodexRolloutFiles([]string{path}, workingDir)
	require.Equal(t, []string{path}, matches, "precondition: the rollout is this launch dir's")
	require.Zero(t, uncorrelated)

	wedgeLaunchDirResolution(t, dir)
	type answer struct {
		matches                  []string
		uncorrelated, unresolved int
		unreadable               error
	}
	got := withinBound(t, func() answer {
		m, u, r, e := classifyCodexRolloutFiles([]string{path}, workingDir)
		return answer{m, u, r, e}
	})
	require.Empty(t, got.matches)
	require.Equal(t, 1, got.uncorrelated, "a cwd af could not resolve is uncorrelated, not another directory's")
	require.Equal(t, 1, got.unresolved, "and counted as a lookup that did not happen, apart from a header still being written")
	require.True(t, errors.Is(got.unreadable, fsprobe.ErrProbeTimedOut), "and reported as a channel af could not look through: %v", got.unreadable)
}

// agentSwapHarness is a running Claude session in a real worktree, with a mock
// tmux that records whether the outgoing agent was killed, ready for an agent
// swap to aider.
type agentSwapHarness struct {
	backend  *LocalBackend
	inst     *Instance
	worktree string
	pty      *recordingPtyFactory
	killed   *atomic.Bool
}

func newAgentSwapHarness(t *testing.T) agentSwapHarness {
	t.Helper()
	globalAiderHome(t)
	aider := filepath.Join(t.TempDir(), "aider")
	require.NoError(t, os.WriteFile(aider, []byte("#!/bin/sh\n"), 0o755))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides[tmux.ProgramAider] = aider
	require.NoError(t, config.SaveConfig(cfg))

	h := agentSwapHarness{backend: &LocalBackend{}, pty: &recordingPtyFactory{t: t}, killed: &atomic.Bool{}}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "kill-session") {
				h.killed.Store(true)
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	repoRoot := initTempGitRepo(t)
	h.worktree = t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoRoot, h.worktree, "wedged-swap", "wedged-swap-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("wedged-swap", tmux.ProgramClaude, h.pty, cmdExec)
	h.inst = &Instance{
		ID: "wedged-swap-id", Title: "wedged-swap", Path: repoRoot, Program: tmux.ProgramClaude,
		backend: h.backend, Tabs: []*Tab{newAgentTab(ts)}, gitWorktree: gw, started: true, liveness: LiveRunning,
	}
	return h
}

// wedgeWorktreeStat makes every stat of path block until the test ends.
func wedgeWorktreeStat(t *testing.T, path string) {
	t.Helper()
	wait := launchDirWedge(t, path)
	t.Cleanup(fsprobe.SetStatPathForTest(func(p string) (os.FileInfo, error) {
		wait(p)
		return os.Stat(p)
	}))
}

// requireWorktreeStalled asserts err is the swap paths' answer to a worktree
// look that did not answer: the WorktreeUnavailableError that says what to do.
func requireWorktreeStalled(t *testing.T, err error) {
	t.Helper()
	var wtErr *WorktreeUnavailableError
	require.ErrorAs(t, err, &wtErr, "a worktree af could not look at fails the swap: %v", err)
	require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the error names the bounded probe: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "and its deadline: %v", err)
	require.Contains(t, err.Error(), "retry the swap", "the error says what to do")
}

// An agent swap's admission preflights the replacement in the worktree before
// SwapAgent is reached, under the daemon's operation lock. On a wedged mount
// that preflight fails the swap within the bound, with the same actionable
// error, rather than holding the lock forever (#7287 review R2). The wedge is
// in place before admission begins.
func TestPrepareAgentSwapDoesNotStallOnAWedgedWorktree(t *testing.T) {
	h := newAgentSwapHarness(t)
	wedgeWorktreeStat(t, h.worktree)
	err := withinBound(t, func() error {
		_, err := h.backend.PrepareAgentSwap(h.inst, tmux.ProgramAider)
		return err
	})
	requireWorktreeStalled(t, err)
	require.False(t, h.killed.Load(), "the running agent is left alone")
	require.Empty(t, h.pty.cmds, "no replacement starts")
}

// An agent swap admitted before the mount wedged checks the worktree again
// before it stops the running agent. That check fails the swap within the
// bound, and the running agent is left alone.
func TestSwapAgentDoesNotStallOnAWedgedWorktree(t *testing.T) {
	h := newAgentSwapHarness(t)
	plan, err := h.backend.PrepareAgentSwap(h.inst, tmux.ProgramAider)
	require.NoError(t, err)

	wedgeWorktreeStat(t, h.worktree)
	requireWorktreeStalled(t, withinBound(t, func() error { return h.backend.SwapAgent(h.inst, plan) }))
	require.False(t, h.killed.Load(), "the running agent is left alone")
	require.Empty(t, h.pty.cmds, "no replacement starts")
}

// A manual account swap preflights every replacement command in the worktree
// before it stops any pane. On a wedged mount that fails the swap within the
// bound, with the same actionable error (#7287 review R2).
func TestManualAccountSwapDoesNotStallOnAWedgedWorktree(t *testing.T) {
	// The proof preflights the replacement's executable, which need not be
	// installed where the test runs.
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramClaude), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
	require.NoError(t, inst.CheckManualAccountSwap("work", "", false), "precondition: the manual swap's proof passes")

	wedgeWorktreeStat(t, inst.GetWorktreePath())
	requireWorktreeStalled(t, withinBound(t, func() error { return inst.ValidateManualAccountSwap("work", "", false) }))
}

// The held-rollout judge (capture's held fallback, the follow loop, an
// operator repair's held observation) correlates a held rollout by the cwd it
// records, usually the launch dir. One that does not resolve within the bound
// leaves the rollout unclassified, so the answer is undecided, never another
// directory's (#7287 review R4).
func TestHeldCodexConversationDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	codexHome := t.TempDir()
	workDir := filepath.Join(testguard.CanonicalTempDir(t), "wt")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	own := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	snap := heldTestInstance(t, own).BindConversationCaptureRuntime(
		beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir))
	conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
	require.NoError(t, err)
	require.Equal(t, heldTestOwnID, conv.ID, "precondition: the held rollout is this launch dir's")

	wedgeLaunchDirResolution(t, workDir)
	type answer struct {
		conv AgentConversationData
		err  error
	}
	got := withinBound(t, func() answer {
		conv, err := HeldAgentConversation(tmux.ProgramCodex, snap)
		return answer{conv, err}
	})
	require.False(t, got.conv.HasID(), "a cwd af could not resolve names no conversation")
	require.True(t, errors.Is(got.err, errCodexCaptureUndecided), "the answer is undecided, never another directory's: %v", got.err)
}

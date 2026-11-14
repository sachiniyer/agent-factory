package session

import (
	"context"
	"errors"
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

// The restore guard asks the attribution rule first, so it reaches its own
// resolution of the launch dir only once the attribution rule's has answered:
// this is a mount that wedges in between. The guard then behaves as if the
// resolution had failed, as one does when an ancestor is unreadable: the
// launch dir as spelled decides.
func TestCodexRestoreGuardDoesNotStallWhenTheLaunchDirWedgesAfterAttribution(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	base := testguard.CanonicalTempDir(t)
	real := filepath.Join(base, "real", "repo")
	require.NoError(t, os.MkdirAll(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(filepath.Dir(real), link))
	workDir := filepath.Join(link, "repo")
	gw, err := sessiongit.NewGitWorktreeFromStorage(t.TempDir(), workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title: "t", Program: tmux.ProgramCodex, gitWorktree: gw,
		Tabs: []*Tab{{Conversation: AgentConversationData{
			Agent: tmux.ProgramCodex, ID: codexBoundRecordedID, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout,
		}}},
	}
	inst.Path = gw.GetRepoPath()
	past := time.Now().Add(-time.Hour)
	// Codex records the kernel's cwd: the resolved spelling, which answers.
	setModTime(t, writeCodexRolloutFileWithCwd(t, codexHome, codexBoundRolloutName(codexBoundRecordedID), real), past)
	setModTime(t, writeCodexRolloutFileWithCwd(t, codexHome, codexBoundRolloutName(codexBoundNewerID), real), past.Add(time.Minute))
	under := func(path string) bool {
		return path == link || strings.HasPrefix(path, link+string(filepath.Separator))
	}

	_, ok, decline := prepareExactResumeConversation(inst, "codex")
	require.False(t, ok, "precondition: with the launch dir answering, the newer thread is this worktree's")
	require.Equal(t, staleCarryReason, decline.reason)

	restore := failLaunchDirResolution(t, link)
	_, failedOK, _ := prepareExactResumeConversation(inst, "codex")
	restore()
	require.True(t, failedOK, "precondition: when the launch dir cannot be resolved, its spelling decides and nothing newer correlates")

	// How many lookups the attribution rule makes of the launch dir, so the
	// mount can wedge right after them.
	var looks atomic.Int32
	restore = fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if under(path) {
			looks.Add(1)
		}
		return filepath.EvalSymlinks(path)
	})
	require.True(t, inst.newerConversationIsEvidence(workDir), "precondition: af owns this worktree")
	restore()
	answered := looks.Load()
	looks.Store(0)
	wait := launchDirWedge(t, link)
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if under(path) && looks.Add(1) > answered {
			wait(path)
		}
		return filepath.EvalSymlinks(path)
	}))

	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "codex")
		return exactResumeAnswer{program, ok, decline}
	})
	require.Greater(t, looks.Load(), answered, "the guard's own resolution reached the wedge")
	require.Equal(t, failedOK, got.ok, "a resolution that did not answer decides as one that failed: %+v", got.decline)
	require.Contains(t, got.program, "resume "+codexBoundRecordedID)
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired on a look that did not happen")
}

// A Codex account-swap carry resolves the launch dir to correlate newer
// rollouts. One that does not answer within the bound refuses the carry: it
// never carries a rollout that may be stale.
func TestCodexCarryRefusesWhenTheLaunchDirDoesNotResolve(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: with the launch dir answering, the rollout carries")

	wedgeLaunchDirResolution(t, inst.GetWorktreePath())
	require.Equal(t, carryRefusalCode(carryUncorrelatedReason), withinBound(t, inst.AccountSwapCarryRefusal),
		"require_carry's check-ahead defers the move")
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	if carry := inst.accountSwapLaunch.carry; carry != nil {
		t.Fatalf("a look that did not happen carried %s", carry.transcript)
	}
	require.Equal(t, carryUncorrelatedReason, inst.accountSwapLaunch.carryFallback, "the swap states why it starts fresh")
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
	matches, uncorrelated, _ := classifyCodexRolloutFiles([]string{path}, workingDir)
	require.Equal(t, []string{path}, matches, "precondition: the rollout is this launch dir's")
	require.Zero(t, uncorrelated)

	wedgeLaunchDirResolution(t, dir)
	type answer struct {
		matches      []string
		uncorrelated int
		unreadable   error
	}
	got := withinBound(t, func() answer {
		m, u, e := classifyCodexRolloutFiles([]string{path}, workingDir)
		return answer{m, u, e}
	})
	require.Empty(t, got.matches)
	require.Equal(t, 1, got.uncorrelated, "a cwd af could not resolve is uncorrelated, not another directory's")
	require.True(t, errors.Is(got.unreadable, fsprobe.ErrProbeTimedOut), "and reported as a channel af could not look through: %v", got.unreadable)
}

// An agent swap checks the worktree exists before it stops the running agent.
// On a wedged mount that check fails the swap within the bound, with an error
// that says what to do, and the running agent is left alone.
func TestSwapAgentDoesNotStallOnAWedgedWorktree(t *testing.T) {
	globalAiderHome(t)
	aider := filepath.Join(t.TempDir(), "aider")
	require.NoError(t, os.WriteFile(aider, []byte("#!/bin/sh\n"), 0o755))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides[tmux.ProgramAider] = aider
	require.NoError(t, config.SaveConfig(cfg))

	ptyFactory := &recordingPtyFactory{t: t}
	var killed atomic.Bool
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), "kill-session") {
				killed.Store(true)
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	repoRoot := initTempGitRepo(t)
	worktreePath := t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(repoRoot, worktreePath, "wedged-swap", "wedged-swap-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("wedged-swap", tmux.ProgramClaude, ptyFactory, cmdExec)
	backend := &LocalBackend{}
	inst := &Instance{
		ID: "wedged-swap-id", Title: "wedged-swap", Path: repoRoot, Program: tmux.ProgramClaude,
		backend: backend, Tabs: []*Tab{newAgentTab(ts)}, gitWorktree: gw, started: true, liveness: LiveRunning,
	}
	plan, err := backend.PrepareAgentSwap(inst, tmux.ProgramAider)
	require.NoError(t, err)

	wait := launchDirWedge(t, worktreePath)
	t.Cleanup(fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		wait(path)
		return os.Stat(path)
	}))
	err = withinBound(t, func() error { return backend.SwapAgent(inst, plan) })
	var wtErr *WorktreeUnavailableError
	require.ErrorAs(t, err, &wtErr, "a worktree af could not look at fails the swap: %v", err)
	require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the error names the bounded probe: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "and its deadline: %v", err)
	require.Contains(t, err.Error(), "retry the swap", "the error says what to do")
	require.False(t, killed.Load(), "the running agent is left alone")
	require.Empty(t, ptyFactory.cmds, "no replacement starts")
}

package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3503: the drain allowance these probes run under, and what it is actually
// for.
//
// Cmd.WaitDelay does NOT bound a slow git. Its timer starts only once the
// context is done or the child has already exited, so what it bounds is the
// parent's wait for the output pipes to reach EOF afterwards. At 100ms that
// budget sat inside the noise of a loaded box — measured max 184ms on a
// 16-core host at load ~95, for a `git rev-parse` that provably forks nothing —
// so ordinary scheduler latency was being reported as a failed repository
// probe.

// withRepoGitWaitDelay drives the production allowance for one test and
// restores it after, so a test asserts a BEHAVIOUR at a stated value instead of
// silently inheriting whatever the constant happens to be today.
func withRepoGitWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	prev := repoGitWaitDelay
	repoGitWaitDelay = d
	t.Cleanup(func() { repoGitWaitDelay = prev })
}

// gitRepoForProbe creates an ordinary checkout with one commit.
func gitRepoForProbe(t *testing.T) string {
	t.Helper()
	root := filepath.Join(testguard.CanonicalTempDir(t), "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	for _, args := range [][]string{{"init"}, {"commit", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return root
}

// installGitWithLingeringHelper puts a git shim on PATH that answers every
// command through the real git, and — only for the first `--show-toplevel`
// probe — leaves a background helper holding the inherited stdout for hold
// after git itself exits.
//
// That is the exact shape WaitDelay exists for: the answer is already written
// and complete, but the pipe cannot reach EOF until the helper lets go.
func installGitWithLingeringHelper(t *testing.T, hold time.Duration) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "held.once")
	pidFile := filepath.Join(binDir, "helper.pids")
	reapShimChildren(t, pidFile)
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--show-toplevel" ] && [ ! -e '%s' ]; then
    : > '%s'
    out=$('%s' "$@") || exit $?
    # The helper inherits this shell's stdout and outlives it, so the parent's
    # read cannot see EOF until it exits.
    sleep %.2f &
    echo $! >> '%s'
    printf '%%s\n' "$out"
    exit 0
  fi
done
exec '%s' "$@"
`, marker, marker, realGit, hold.Seconds(), pidFile, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reapShimChildren kills every pid a git shim recorded in pidFile.
//
// The shims here deliberately outlive the git process that spawned them — that
// is the condition under test — and exec kills only the shim shell it started,
// never the shell's own children. Without this, each invocation leaks a sleep
// onto a shared machine and `-count=N` multiplies it (#3594 review). Best
// effort: a pid that has already exited is the normal case.
func reapShimChildren(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(raw)) {
			if pid, convErr := strconv.Atoi(field); convErr == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// TestRepoProbeKeepsAnAnswerAHelperOnlyBrieflyDelayed is the #3503 regression.
//
// Git has already written the complete, correct toplevel; a helper holds the
// pipe open a little longer. At the production allowance that answer must
// survive. The subtest pins WHY the value moved: at the old 100ms the identical
// run threw the answer away, which is not fail-closed — it is lossy, and on a
// loaded box it happened with no helper involved at all.
func TestRepoProbeKeepsAnAnswerAHelperOnlyBrieflyDelayed(t *testing.T) {
	root := gitRepoForProbe(t)

	t.Run("production allowance keeps the answer", func(t *testing.T) {
		installGitWithLingeringHelper(t, 400*time.Millisecond)
		repo, err := RepoFromPath(root)
		require.NoError(t, err,
			"the probe held git's complete answer; a helper holding the pipe for 400ms "+
				"must not turn that into a failed repository resolution (#3503)")
		assert.Equal(t, root, repo.IdentityPath())
	})

	t.Run("the old 100ms allowance discarded it", func(t *testing.T) {
		withRepoGitWaitDelay(t, 100*time.Millisecond)
		installGitWithLingeringHelper(t, 400*time.Millisecond)
		_, err := RepoFromPath(root)
		require.Error(t, err,
			"documents the behaviour this change removes: at 100ms the same complete "+
				"answer was abandoned mid-read")
		assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
			"#3500 keeps that failure honest, but honest is not the same as correct")
	})
}

// TestRepoProbeWaitDelayFitsTheCallersDeadline pins the axis the allowance is
// chosen on (#3503, question 2). Not which git command runs — since #3500 every
// probe here is load-bearing, because an unanswered one fails the whole
// resolution instead of falling back — but whether the caller promised a budget.
func TestRepoProbeWaitDelayFitsTheCallersDeadline(t *testing.T) {
	t.Run("no deadline gets the full allowance", func(t *testing.T) {
		assert.Equal(t, repoGitWaitDelay, repoProbeWaitDelay(context.Background()),
			"RepoFromPath and CurrentRepo have no budget to overrun, so nothing is "+
				"bought by being stingy here")
	})

	t.Run("a deadline bounds the overrun by the caller's own budget", func(t *testing.T) {
		// The shape ResolveRegisteredProjectRepoID uses: 250ms per probe inside
		// a 1s registry scan.
		ctx, cancel := context.WithTimeout(context.Background(), registeredProjectProbeTimeout)
		defer cancel()
		got := repoProbeWaitDelay(ctx)

		// State the guarantee exactly. WaitDelay's timer starts when the context
		// is done, so this allowance is ADDED to the deadline rather than carved
		// out of it — the worst case is deadline + allowance. An earlier draft of
		// this test claimed the drain "fits inside" the caller's budget, which is
		// false (#3594 review).
		assert.LessOrEqual(t, registeredProjectProbeTimeout+got, 2*registeredProjectProbeTimeout,
			"the overrun must be proportional to what the caller asked for")
		assert.Less(t, got, repoGitWaitDelay,
			"and nothing may drag a 250ms caller toward the 2s default: that is the "+
				"whole point of deriving the allowance from the caller")
		assert.Greater(t, got, time.Duration(0))
	})

	t.Run("an almost-elapsed deadline still bounds the drain", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		got := repoProbeWaitDelay(ctx)
		assert.Equal(t, minRepoProbeWaitDelay, got)
		assert.NotZero(t, got,
			"WaitDelay of 0 means NO bound at all — a caller must never be able to "+
				"arithmetic its way into disabling the guard")
	})

	t.Run("a deadline beyond the default does not inflate it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		assert.Equal(t, repoGitWaitDelay, repoProbeWaitDelay(ctx))
	})
}

// TestRepoGitWaitDelayClearsMeasuredDrainLatency guards the number itself
// against being quietly tightened back.
//
// The measurement it encodes: over 300 `git rev-parse --show-toplevel` runs at
// load ~95, the gap from git's last write to Wait returning reached 184ms, with
// strace showing rev-parse performs exactly one execve and never forks — so no
// helper could have been holding anything. Any allowance inside that range
// reports a probe that in fact succeeded as one that never answered.
func TestRepoGitWaitDelayClearsMeasuredDrainLatency(t *testing.T) {
	assert.GreaterOrEqual(t, repoGitWaitDelay, time.Second,
		"the observed drain tail was 184ms on an ordinary loaded box; an allowance "+
			"near it turns scheduler latency into a failed repository probe (#3503)")
}

// TestRepoProbeWaitDelayDoesNotRescueAWedgedGit records the limit honestly, so
// the constant is never again defended by a protection it does not provide.
//
// WaitDelay's timer starts only once the context is done or the child has
// exited. On the unbounded entry points a git wedged on a stale mount does
// neither, so no value here bounds that hang; only a caller deadline does.
func TestRepoProbeWaitDelayDoesNotRescueAWedgedGit(t *testing.T) {
	withRepoGitWaitDelay(t, 100*time.Millisecond)
	// The sleep runs in the BACKGROUND and its pid is recorded, because
	// CommandContext kills the shim shell and not the shell's foreground child —
	// a bare `sleep 30` here would outlive the test (#3594 review).
	pidFile := filepath.Join(t.TempDir(), "wedged.pids")
	reapShimChildren(t, pidFile)
	installFakeGit(t, fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! >> '%s'\nwait\n", pidFile))

	// A caller deadline is what actually bounds it — the same shim with no
	// deadline would block far past this.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { _, err := RepoFromPathContext(ctx, t.TempDir()); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrRepoProbeUnanswered)
		assert.ErrorIs(t, err, context.DeadlineExceeded,
			"the caller's deadline is the thing that ended it, and the chain must say so")
	case <-time.After(15 * time.Second):
		t.Fatal("a caller deadline must bound a wedged git even though WaitDelay cannot")
	}
}

// TestReapShimChildrenActuallyKillsThem verifies the cleanup these tests rely
// on, rather than assuming it. Counting stray processes after a run cannot
// attribute them on a shared machine — other work spawns sleeps too — so the
// reaper is exercised directly: the subtest's cleanup must have killed the
// recorded child by the time t.Run returns.
func TestReapShimChildrenActuallyKillsThem(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "probe.pids")
	var pid int

	t.Run("child recorded and reaped", func(t *testing.T) {
		reapShimChildren(t, pidFile)
		cmd := exec.Command("sleep", "300")
		require.NoError(t, cmd.Start())
		pid = cmd.Process.Pid
		require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600))
		require.NoError(t, syscall.Kill(pid, 0), "fixture: the child must be alive here")
		go func() { _ = cmd.Wait() }() // reap the zombie; the kill is what is under test
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reapShimChildren left pid %d alive: every shim child these tests "+
				"record must be gone when the test that spawned it ends, or a shared "+
				"machine accumulates them across runs (#3594 review)", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

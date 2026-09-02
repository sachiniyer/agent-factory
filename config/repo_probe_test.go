package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the #3500 classification at the boundary that owns the git
// child: a resolution failure is an ANSWER about the path only when git
// actually produced one. A probe that never completed — killed, unstartable,
// or abandoned mid-read — must be reportable as exactly that, so no caller
// converts "could not establish" into a verdict (the rule #3371 and #3478
// already applied to the tmux -N probe and to sandbox teardown).

// installFakeGit puts a git shim first on PATH. The real directories stay on
// PATH behind it so the shim's own shell utilities still resolve.
func installFakeGit(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRepoFromPathClassifiesAbandonedProbeAsUnanswered reproduces the shape
// reported in #3500: a helper outliving git holds the output pipe open, the
// WaitDelay expires, and the read is abandoned. Nothing about the path was
// learned, and the error must say so.
//
// The allowance is driven explicitly and the helper holds the pipe far past it
// (#3503). This test is about the CLASSIFICATION of an abandoned read, so it
// must not also depend on the production value — when that value rose to 2s the
// original `sleep 2` fake became a race against the very bound it was meant to
// trip.
func TestRepoFromPathClassifiesAbandonedProbeAsUnanswered(t *testing.T) {
	withRepoGitWaitDelay(t, 100*time.Millisecond)
	// The helper's pid is recorded and reaped: exec kills the shim shell, never
	// the shell's own children, so an unreaped helper outlives the test on a
	// shared machine (#3594 review).
	pidFile := filepath.Join(t.TempDir(), "helper.pids")
	reapShimChildren(t, pidFile)
	installFakeGit(t, fmt.Sprintf(`#!/bin/sh
# A background helper inherits git's stdout and outlives it, so the pipe never
# reaches EOF and the parent's WaitDelay expires before the read completes.
sleep 30 &
echo $! >> '%s'
printf '%%s\n' "/not/read"
exit 0
`, pidFile))

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"a probe abandoned when WaitDelay expired never answered; it must not be reportable as a fact about the path")
	assert.NotErrorIs(t, err, ErrNotGitRepository,
		"an unanswered probe is not an outside-repository answer")
	assert.Contains(t, err.Error(), "WaitDelay",
		"the message must still name the subprocess outcome that actually happened")
}

// TestRepoFromPathClassifiesSignalKilledProbeAsUnanswered: a git killed by a
// signal exited without a diagnostic, so its stderr says nothing about the
// repository. ProcessState reports a negative exit code for that.
func TestRepoFromPathClassifiesSignalKilledProbeAsUnanswered(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
kill -9 $$
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"a signal-killed probe never answered")
	assert.NotErrorIs(t, err, ErrNotGitRepository, "a signal is not an answer")
}

// TestRepoFromPathClassifiesMissingGitAsUnanswered: with no git on PATH the
// question was never put to anything. Persistent, but still not a claim the
// caller may make about the path.
func TestRepoFromPathClassifiesMissingGitAsUnanswered(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"git that cannot be started answered nothing")
}

// TestRepoFromPathKeepsAnsweredFailuresOutOfTheUnansweredClass is the other
// half of the split, and the one that keeps the classifier from swallowing
// every failure: git ran, diagnosed, and exited. That IS an answer about the
// path, and the existing outside-repository classification must survive
// unchanged.
func TestRepoFromPathKeepsAnsweredFailuresOutOfTheUnansweredClass(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
exit 128
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotGitRepository,
		"git answered, and the answer is no — that classification must not change")
	assert.NotErrorIs(t, err, ErrRepoProbeUnanswered,
		"an answered probe must never be reported as unanswered, or the honest message becomes noise")
}

// TestRepoFromPathClassifiesForkExecFailureAsUnanswered: git is found on PATH
// but cannot be executed at all. This produces an *fs.PathError — matching
// NEITHER *exec.Error nor *exec.ExitError — which is why the classifier proves
// an answer instead of enumerating failures (#3500 review). The same shape
// reaches a box that is out of process slots, which is exactly the load this
// bug was reported under.
func TestRepoFromPathClassifiesForkExecFailureAsUnanswered(t *testing.T) {
	binDir := t.TempDir()
	// Executable, but not a program: no shebang and no valid binary format, so
	// execve fails with ENOEXEC after LookPath succeeds.
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte("not a program\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"a git that could not be executed answered nothing")
	assert.NotErrorIs(t, err, ErrNotGitRepository, "a failed exec is not an answer")
}

// TestRepoFromPathClassifiesAKilledBareProbeAsUnanswered: a bare repository has
// no work tree, so its toplevel probe fails cleanly and the bare shape is
// settled by a SECOND probe. When that one is killed, nothing established
// whether the path is a repository — even though the first failure was a
// perfectly good answer to a different question (#3500 review).
func TestRepoFromPathClassifiesAKilledBareProbeAsUnanswered(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	--is-bare-repository|--absolute-git-dir) kill -9 $$ ;;
	esac
done
printf '%s\n' 'fatal: this operation must be run in a work tree' >&2
exit 128
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"the probe that would have recognised a bare repository was killed; the question is open")
}

// TestRepoFromPathKeepsAnsweredBareRefusalOutOfTheUnansweredClass is the guard
// on the case above: when the bare probe ANSWERS (an ordinary directory, where
// it reports a non-repository just as the first probe did), the classification
// must be unchanged. Otherwise the bare handling would quietly turn every
// answered refusal into an unknown.
func TestRepoFromPathKeepsAnsweredBareRefusalOutOfTheUnansweredClass(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
exit 128
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrRepoProbeUnanswered,
		"both probes answered; nothing here is unknown")
	assert.ErrorIs(t, err, ErrNotGitRepository)
}

// TestRepoFromPathClassifiesAKilledWorktreeProbeAsUnanswered: the toplevel
// probe answers, and the SECOND probe — the one that tells a linked worktree
// from a main one — is killed. Its answered failure has a documented fallback
// (treat the toplevel as the identity root); an unanswered one must not take
// it, because for a linked worktree that fallback silently splits one
// repository's ID in two (#3500 review).
func TestRepoFromPathClassifiesAKilledWorktreeProbeAsUnanswered(t *testing.T) {
	toplevel := t.TempDir()
	t.Setenv("AF_TEST_TOPLEVEL", toplevel)
	installFakeGit(t, `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	--git-dir) kill -9 $$ ;;
	--show-toplevel) printf '%s\n' "$AF_TEST_TOPLEVEL"; exit 0 ;;
	esac
done
exit 1
`)

	_, err := RepoFromPath(toplevel)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"the probe that would have identified a linked worktree was killed; its fallback must not be taken on that")
}

// TestRepoFromPathKeepsTheAnsweredWorktreeFallback is the guard on the case
// above: when that second probe ANSWERS with an error, the documented
// main-worktree fallback still applies and resolution still succeeds.
func TestRepoFromPathKeepsTheAnsweredWorktreeFallback(t *testing.T) {
	toplevel := t.TempDir()
	t.Setenv("AF_TEST_TOPLEVEL", toplevel)
	installFakeGit(t, `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	--show-toplevel) printf '%s\n' "$AF_TEST_TOPLEVEL"; exit 0 ;;
	esac
done
printf '%s\n' 'fatal: this version does not know that' >&2
exit 128
`)

	repo, err := RepoFromPath(toplevel)
	require.NoError(t, err, "an answered probe keeps its fallback; only an unanswered one is fatal")
	assert.Equal(t, toplevel, repo.Root)
	assert.Equal(t, toplevel, repo.IdentityPath())
}

// TestRepoFromPathRejectsAStderrVerdictFromAKilledProbe: the definite
// outside-repository verdict is read out of git's stderr, and that text is only
// as good as the process that produced it. A git killed after writing the
// diagnostic but before exiting proves nothing — and that branch was the one
// place a verdict could be returned without consulting the classifier at all
// (#3500 review round 4).
func TestRepoFromPathRejectsAStderrVerdictFromAKilledProbe(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
kill -9 $$
`)

	_, err := RepoFromPath(t.TempDir())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotGitRepository,
		"a diagnostic from a process that never exited is not a verdict about the path")
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"a killed probe is unanswered however far it got before dying")
}

// TestRepoFromPathContextNamesTheCallersDeadline: when the caller's own
// deadline is what killed the probe, exec reports "signal: killed" and the
// context error is nowhere in the chain, so callers cannot tell a timeout from
// any other death (#3517). Classification still comes from the command outcome
// — the deadline only supplies the CAUSE once that outcome is unanswered.
func TestRepoFromPathContextNamesTheCallersDeadline(t *testing.T) {
	// Backgrounded and reaped for the same reason as above: a foreground child
	// survives the shim shell exec kills at the deadline (#3594 review).
	deadlinePids := filepath.Join(t.TempDir(), "deadline.pids")
	reapShimChildren(t, deadlinePids)
	installFakeGit(t, fmt.Sprintf(`#!/bin/sh
sleep 5 &
echo $! >> '%s'
wait
`, deadlinePids))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := RepoFromPathContext(ctx, t.TempDir())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"a caller that owns the cancellation must be able to detect its own timeout")
	assert.ErrorIs(t, err, ErrRepoProbeUnanswered,
		"and it is still an unanswered probe, not a verdict on the path")
}

// TestRepoFromPathDoesNotBlameTheContextForAnAnsweredFailure is the guard on
// the ordering above: a deadline that expires around an answered probe must not
// turn that answer into a timeout. Here git answers immediately and the context
// is already dead by the time the error is classified.
func TestRepoFromPathDoesNotBlameTheContextForAnAnsweredFailure(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
exit 128
`)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := RepoFromPathContext(ctx, t.TempDir())
	cancel()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotGitRepository, "git answered; the answer stands")
	assert.NotErrorIs(t, err, ErrRepoProbeUnanswered)
}

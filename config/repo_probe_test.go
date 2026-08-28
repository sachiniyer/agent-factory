package config

import (
	"os"
	"path/filepath"
	"testing"

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
// 100ms WaitDelay expires, and the read is abandoned. Nothing about the path
// was learned, and the error must say so.
func TestRepoFromPathClassifiesAbandonedProbeAsUnanswered(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
# A background helper inherits git's stdout and outlives it, so the pipe never
# reaches EOF and the parent's WaitDelay expires before the read completes.
sleep 2 &
printf '%s\n' "/not/read"
exit 0
`)

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

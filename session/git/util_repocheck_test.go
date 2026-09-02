package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installFakeGit puts a git shim first on PATH, with the real directories behind
// it so the shim's own utilities still resolve.
func installFakeGit(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCheckGitRepoKeepsTheReasonABoolCannotHold is the #3504 regression for the
// bool: IsGitRepo collapses "git says no" and "git never answered" into false,
// and its callers then tell a user their path is not a repository. CheckGitRepo
// keeps the difference so they can word it honestly.
func TestCheckGitRepoKeepsTheReasonABoolCannotHold(t *testing.T) {
	installFakeGit(t, `#!/bin/sh
kill -9 $$
`)
	dir := t.TempDir()

	err := CheckGitRepo(dir)
	require.Error(t, err)
	assert.True(t, config.RepoProbeUnanswered(err),
		"a killed probe must be reportable as unanswered, not as a verdict about the path")
	assert.False(t, IsGitRepo(dir),
		"the bool keeps its existing meaning for callers that only need a decision")
}

// TestCheckGitRepoStillAnswersForARealRefusal guards the other direction: an
// ordinary non-repository must keep producing a definite answer, or every
// caller would start hedging about paths git is perfectly clear on.
func TestCheckGitRepoStillAnswersForARealRefusal(t *testing.T) {
	dir := t.TempDir()

	err := CheckGitRepo(dir)
	require.Error(t, err, "a plain directory is not a git repository")
	assert.False(t, config.RepoProbeUnanswered(err), "git answered; the answer is no")
	assert.False(t, IsGitRepo(dir))
}

// TestCheckGitRepoAcceptsARealRepository: the success path is unchanged.
func TestCheckGitRepoAcceptsARealRepository(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "master", ".")

	assert.NoError(t, CheckGitRepo(dir))
	assert.True(t, IsGitRepo(dir))
}

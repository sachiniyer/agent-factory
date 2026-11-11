package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// #7079: a hook run whose project config cannot be read in time used to skip
// post_worktree_commands with a log line alone, so a worktree went into use
// without its setup and nothing on the row said so. Not reading the config is
// not the same as it listing no commands.

// unreadableHookProject is a registered project whose post_worktree_commands
// touch a marker, behind a git whose checkout-identity probe — the read #6748
// bounds with its 1s scan — sleeps past that budget while the returned flag
// file exists.
func unreadableHookProject(t *testing.T) (repo, marker, slowFlag string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo = filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initCmd := exec.Command("git", "init", "--quiet")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	repo, err = filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	_, err = config.RegisterProject(repo)
	require.NoError(t, err)

	marker = filepath.Join(t.TempDir(), "setup-ran")
	writeLegacyRepoConfig(t, config.RepoIDFromRoot(repo), &config.RepoConfig{
		PostWorktreeCommands: []string{"touch " + marker},
	})

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	slowFlag = filepath.Join(t.TempDir(), "slow")
	require.NoError(t, os.WriteFile(slowFlag, nil, 0o600))
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -eq 4 ] && [ "$3" = rev-parse ] && [ "$4" = --git-common-dir ] && [ -e %q ]; then
	sleep 3
fi
exec %q "$@"
`, slowFlag, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return repo, marker, slowFlag
}

func noHookRetryWait(t *testing.T, wait func()) {
	t.Helper()
	prev := hookConfigRetryWait
	hookConfigRetryWait = wait
	t.Cleanup(func() { hookConfigRetryWait = prev })
}

// The run that cannot read its config records a note the session puts on the
// row (SetupFailureNote), after spending a launch's three attempts.
func TestHookRunThatCannotReadItsConfigSaysSoOnTheRow(t *testing.T) {
	repo, marker, _ := unreadableHookProject(t)
	noHookRetryWait(t, func() {})
	gw, err := NewGitWorktreeFromStorage(repo, t.TempDir(), "hooks-unknown", "main", "", true, false)
	require.NoError(t, err)

	<-gw.runHooks()

	require.NoFileExists(t, marker, "precondition: the commands did not run")
	note := gw.SetupFailureNote()
	require.Contains(t, note, "post_worktree_commands did not run", "the row says the setup did not run")
	require.Contains(t, note, "in time (3 attempts)")
	require.Contains(t, note, repo)
}

// A read that answers on a later attempt runs the commands: slow is not absent.
func TestHookRunRetriesAConfigReadThatWentUnanswered(t *testing.T) {
	repo, marker, slowFlag := unreadableHookProject(t)
	noHookRetryWait(t, func() { _ = os.Remove(slowFlag) }) // the box answers again
	gw, err := NewGitWorktreeFromStorage(repo, t.TempDir(), "hooks-retry", "main", "", true, false)
	require.NoError(t, err)

	<-gw.runHooks()

	require.FileExists(t, marker, "a config that answers on retry runs the project's setup commands")
	require.Empty(t, gw.SetupFailureNote())
}

package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitEnv is the minimal identity git needs to commit in the hermetic test repos.
var gitEnv = []string{
	"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
	"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitEnv...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}

// shortenNetworkTimeout temporarily lowers networkGitTimeout so the stalled-fetch
// tests resolve in ~a second instead of the production 60s, restoring it after.
func shortenNetworkTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := networkGitTimeout
	networkGitTimeout = d
	t.Cleanup(func() { networkGitTimeout = orig })
}

// stallOriginViaFakeSSH points the repo's origin at an ssh:// remote whose
// transport is a fake ssh that sleeps far longer than any test timeout. A
// `git fetch origin` then blocks in our control without touching the real
// network, simulating the hung connection from #896.
func stallOriginViaFakeSSH(t *testing.T, repoRoot string) {
	t.Helper()
	fakeSSH := filepath.Join(t.TempDir(), "hang-ssh.sh")
	require.NoError(t, os.WriteFile(fakeSSH, []byte("#!/bin/sh\nsleep 120\n"), 0o755))
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)
	runGit(t, repoRoot, "remote", "add", "origin", "ssh://git@stalled.invalid/repo.git")
}

// TestRunGitNetworkCommand_TimesOutOnStalledFetch is the core #896 regression:
// a fetch from a stalled remote must return a timeout error within the bound
// instead of blocking forever.
func TestRunGitNetworkCommand_TimesOutOnStalledFetch(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	stallOriginViaFakeSSH(t, repoRoot)
	shortenNetworkTimeout(t, time.Second)

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		out, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		require.Error(t, r.err, "a stalled fetch must surface a timeout error")
		assert.Contains(t, r.err.Error(), "timed out")
		assert.Less(t, time.Since(start), 30*time.Second,
			"fetch should be killed at the timeout, not wait for the fake ssh to exit")
	case <-time.After(20 * time.Second):
		t.Fatal("runGitNetworkCommand hung past the timeout on a stalled fetch (#896)")
	}
}

// TestResolveOriginHead_DoesNotHangOnStalledFetch exercises the exact
// session-creation path the bug report names: resolveOriginHead fetches first,
// then falls back to local refs. With a stalled remote it must still return
// promptly (best-effort: empty string when no origin refs are cached).
func TestResolveOriginHead_DoesNotHangOnStalledFetch(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	stallOriginViaFakeSSH(t, repoRoot)
	shortenNetworkTimeout(t, time.Second)

	gw := &GitWorktree{repoPath: repoRoot}

	done := make(chan string, 1)
	start := time.Now()
	go func() { done <- gw.resolveOriginHead() }()

	select {
	case ref := <-done:
		// No origin refs were fetched (the remote stalled), so the best-effort
		// resolution yields an empty string — and crucially, it returned.
		assert.Empty(t, ref)
		assert.Less(t, time.Since(start), 30*time.Second)
	case <-time.After(20 * time.Second):
		t.Fatal("resolveOriginHead hung on a stalled fetch during session creation (#896)")
	}
}

// TestRunGitNetworkCommand_NormalFetchSucceeds guards the happy path: a healthy
// fetch (here from a local filesystem remote, fully hermetic) returns without
// error and is unaffected by the timeout machinery.
func TestRunGitNetworkCommand_NormalFetchSucceeds(t *testing.T) {
	sandboxHome(t)

	// A source repo with a commit acts as origin.
	origin := createGitRepo(t)
	runGit(t, origin, "commit", "--allow-empty", "-m", "initial")

	// A separate repo whose origin points at the source on the local
	// filesystem — no network, but the same `git fetch origin` code path.
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "remote", "add", "origin", origin)

	out, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
	require.NoError(t, err, "a healthy fetch must succeed: %s", out)
}

// TestRunGitNetworkCommand_NoFalseTimeoutWhenGitSucceeds verifies that a
// successful git command does not return a timeout error even if the context
// deadline expires during pipe cleanup. This guards against the race where git
// exits before the deadline but a transport child holds stdout open, so
// cmd.Output returns exec.ErrWaitDelay which runGitCommandContext converts to a
// nil error; runGitNetworkCommand must not then report a false timeout (#914).
func TestRunGitNetworkCommand_NoFalseTimeoutWhenGitSucceeds(t *testing.T) {
	sandboxHome(t)

	// Create a repo and a local origin to fetch from (hermetic, no network).
	origin := createGitRepo(t)
	runGit(t, origin, "commit", "--allow-empty", "-m", "initial")

	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "remote", "add", "origin", origin)

	// A tight timeout: longer than the local fetch needs, but short enough to
	// keep the test fast.
	shortenNetworkTimeout(t, 5*time.Second)

	// Run the fetch repeatedly to widen the window for the (rare) ErrWaitDelay
	// race; with the err != nil guard every successful fetch is error-free.
	for i := 0; i < 10; i++ {
		out, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
		require.NoError(t, err, "a successful fetch must not return a timeout error: %s", out)
	}
}

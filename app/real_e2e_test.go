package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// Real-backend integration tests.
//
// These exercise the *actual* LocalBackend against real tmux + a real git
// repo. The goal is a ground-truth check that FakeBackend's contract
// matches reality — every other e2e test in this package fakes the backend,
// so if the real LocalBackend drifted (e.g. Start returned without actually
// creating a worktree), the faked tests would silently pass while the app
// broke.
//
// Tests skip cleanly when tmux / git / sh are missing so non-unix CI is
// unaffected. Each test creates its own AGENT_FACTORY_HOME tempdir so
// nothing leaks into the developer's real config.
// ----------------------------------------------------------------------------

// skipIfRealBackendDepsMissing skips when the tools LocalBackend needs
// aren't on PATH. tmux is the big one; git is always required; we also need
// a POSIX shell for the stub program.
func skipIfRealBackendDepsMissing(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"tmux", "git", "sh", "cat"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found on PATH — skipping real-backend test", bin)
		}
	}
}

// setupRealRepo makes a git repo in a tempdir with an initial commit, so
// `git worktree add` has a base commit to branch from. Returns the repo
// path.
func setupRealRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGit(t, dir, "init")
	mustRunGit(t, dir, "config", "--local", "user.email", "test@real-e2e.local")
	mustRunGit(t, dir, "config", "--local", "user.name", "Real E2E")
	// commit.gpgsign and tag.gpgsign can inherit from global config and
	// break `git commit` in CI environments without signing set up.
	mustRunGit(t, dir, "config", "--local", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("real e2e\n"), 0644))
	mustRunGit(t, dir, "add", "README.md")
	mustRunGit(t, dir, "commit", "-m", "initial")
	return dir
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
}

// ----------------------------------------------------------------------------
// Session-level smoke test — no TUI, direct NewInstance / Start / Kill.
// ----------------------------------------------------------------------------

// TestRealLocalBackend_FullLifecycle creates a session with the real
// LocalBackend, verifies tmux + worktree exist on disk, kills it, and
// verifies both are cleaned up.
//
// This is the lowest-level "is FakeBackend lying to us?" check. If this
// test fails, the bug is in LocalBackend or our understanding of its
// contract — not in our async plumbing.
func TestRealLocalBackend_FullLifecycle(t *testing.T) {
	skipIfRealBackendDepsMissing(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoDir := setupRealRepo(t)

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "smoke",
		Path:    repoDir,
		Program: "cat", // blocks on stdin, keeps the tmux session alive
	})
	require.NoError(t, err)
	require.Equal(t, "local", inst.GetBackend().Type(),
		"sanity: factory should produce a LocalBackend when ForceRemote is false")

	// Best-effort cleanup so a failing assertion doesn't leak tmux
	// sessions or worktree dirs.
	t.Cleanup(func() {
		if inst.Started() {
			_ = inst.Kill()
		}
	})

	require.NoError(t, inst.Start(true), "Start(true) should succeed for a real local backend")
	require.True(t, inst.Started())

	// Worktree must actually exist on disk — if this fails, LocalBackend is
	// silently skipping worktree creation.
	wt, err := inst.GetGitWorktree()
	require.NoError(t, err)
	wtPath := wt.GetWorktreePath()
	require.NotEmpty(t, wtPath)
	assert.DirExists(t, wtPath, "git worktree directory must exist on disk after Start")

	// Tmux session must be reachable.
	assert.True(t, inst.TmuxAlive(), "tmux session should report alive right after Start")

	// Preview should return without erroring (may be empty since cat has
	// nothing to print — we just want no crash).
	_, err = inst.Preview()
	assert.NoError(t, err)

	// Kill should clean up tmux + worktree.
	require.NoError(t, inst.Kill())
	assert.False(t, inst.TmuxAlive(), "tmux session must be gone after Kill")
	assert.NoDirExists(t, wtPath, "worktree directory must be removed after Kill")
}

// ----------------------------------------------------------------------------
// TUI-level happy-path — end-to-end through teatest, with the real
// LocalBackend. This is the test I'd run to feel confident before merge.
// ----------------------------------------------------------------------------

// newRealE2EHarness is like newE2EHarness but does NOT swap the backend
// factory — NewInstance produces a real LocalBackend. It still swaps the
// PR fetcher so we don't shell out to `gh` (which needs auth).
//
// The caller must have set up a real git repo and chdir'd to it before
// calling start(), because startNewInstance uses Path: ".".
func newRealE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()
	h := newTestHome(t)
	eh := &e2eHarness{t: t, home: h}

	// Stub PR fetcher: return no PR, zero log noise. Don't count calls —
	// we don't care here, the PR behaviour is covered by the faked tests.
	restoreFetcher := SetPRInfoFetcherForTest(func(repoPath, branch string) (*git.PRInfo, error) {
		return nil, nil
	})
	t.Cleanup(restoreFetcher)

	// Stub program: `cat` hangs on stdin, so the tmux session stays alive
	// indefinitely (until we kill it) without needing Claude installed.
	h.program = "cat"

	return eh
}

// The TUI-level test TestRealTUI_CreateThroughKeypresses lives in
// real_tui_e2e_test.go (gated with //go:build !race) because it surfaces a
// pre-existing production race on Instance.Branch — out of scope for the
// async-issues PR but worth fixing separately.

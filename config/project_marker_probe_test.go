package config

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3599: the checkout-marker read is a GIT PROBE, and it had no deadline.
//
// ProjectCheckoutMatches resolves the recorded path's binding before it can
// find the marker at all, and that resolution forks git up to four times. The
// daemon's re-attribution probe runs it against a path that may be wedged on a
// stale mount, while a repository it has already published as its candidate
// stays fail-closed and the repository the path may have flipped TO stays
// ungated — for as long as the read takes, which was forever.
//
// So the read needs a caller-owned deadline, and the failure it produces has to
// land on the right side of #3500's line: "we could not ask git" (which holds
// the identity unknowable), never "git answered no" (which is a verdict and
// releases what it contradicts).

// TestProjectCheckoutMatchesContextBoundsAWedgedGit is the #3599 regression. A
// git that never exits must cost the caller its own deadline and no more, and
// the error must say the question went unanswered rather than reporting the
// checkout as a stranger's.
func TestProjectCheckoutMatchesContextBoundsAWedgedGit(t *testing.T) {
	root := gitRepoForProbe(t)
	// The sleep runs in the BACKGROUND with its pid recorded: CommandContext
	// kills the shim shell, never the shell's foreground child, so a bare
	// `sleep 30` here would outlive the test on a shared box (#3594 review).
	pidFile := filepath.Join(t.TempDir(), "wedged.pids")
	reapShimChildren(t, pidFile)
	installFakeGit(t, fmt.Sprintf("#!/bin/sh\nsleep 30 &\necho $! >> '%s'\nwait\n", pidFile))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type outcome struct {
		matches bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		matches, err := ProjectCheckoutMatchesContext(ctx, root, "chk_0123456789abcdef0123456789abcdef")
		done <- outcome{matches: matches, err: err}
	}()

	select {
	case got := <-done:
		require.Error(t, got.err,
			"a probe the caller's deadline killed established NOTHING about the marker; "+
				"reporting a clean 'does not match' there is a verdict the read never earned (#3500)")
		assert.False(t, got.matches)
		assert.ErrorIs(t, got.err, ErrRepoProbeUnanswered,
			"the daemon branches on this to hold the identity unknowable rather than "+
				"settle a mismatch that releases the gate (#3599)")
		assert.ErrorIs(t, got.err, context.DeadlineExceeded,
			"and the chain must name what ended it, so a stalled mount is not read as "+
				"broken repository metadata (#3517)")
	case <-time.After(15 * time.Second):
		t.Fatal("the marker read must be bounded by the caller's deadline: without one it " +
			"waits out the mount, leaving a flipped-to repository ungated for the whole " +
			"of it (#3599)")
	}
}

// TestProjectCheckoutMatchesContextStillAnswers pins the other half: a deadline
// on the read must not cost it its ordinary verdicts. Both are load-bearing —
// the match is what re-attributes a project, and the proven MISMATCH is what
// keeps a stranger's clone at a reused path from inheriting its personal layer.
func TestProjectCheckoutMatchesContextStillAnswers(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	root := gitRepoForProbe(t)
	project, err := RegisterProject(root)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	matches, err := ProjectCheckoutMatchesContext(ctx, root, project.CheckoutID)
	require.NoError(t, err)
	assert.True(t, matches, "the registered checkout still carries its own marker")

	matches, err = ProjectCheckoutMatchesContext(ctx, root, "chk_ffffffffffffffffffffffffffffffff")
	require.NoError(t, err)
	assert.False(t, matches,
		"and a marker that is present and DIFFERENT is a real answer — the one negative "+
			"the daemon is entitled to settle on")
}

// TestResolveProjectBindingContextResolvesTheSameBinding pins the one thing the
// split may not change: threading a context through must alter WHEN the
// resolution gives up and nothing about what it resolves. The two forms exist
// because the admission paths (register, rebind, selector resolution)
// deliberately keep an unbounded caller lifetime, exactly as RepoFromPath does,
// while a caller that promised a budget gets one.
func TestResolveProjectBindingContextResolvesTheSameBinding(t *testing.T) {
	root := gitRepoForProbe(t)
	binding, err := resolveProjectBinding(root)
	require.NoError(t, err)
	assert.Equal(t, root, binding.root)

	deadlined, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fromCtx, err := resolveProjectBindingContext(deadlined, root)
	require.NoError(t, err)
	assert.Equal(t, binding, fromCtx,
		"the two entry points must differ only in cancellation, never in what they resolve")
}

// TestResolveProjectBindingContextBoundsAWedgedIdentityReCheck is the regression
// for the bound loss #3530 introduced into #3599's deadline-bounded resolver.
//
// resolveProjectBindingContext carries the caller's ctx to every git probe it
// forks itself, but the identity re-check at the end of resolution — added by
// #3530 to refuse a binding whose path changed repositories between the two
// probe batches — used RepoFromPath, which is RepoFromPathContext(context.Background(),
// ...). A git wedged inside that re-check ran under a context that is never done,
// so the caller's deadline could not cancel it and WaitDelay's timer never
// started. The daemon's re-attribution probe, which reaches this function
// through ProjectCheckoutMatchesContext with a per-step deadline, would then
// wedge forever holding the record's probe slot — the exact state #3599 was
// written to eliminate.
//
// The wedge lands precisely inside the identity re-check: a git shim passes
// every probe through to the real git except the SECOND pure
// `rev-parse --show-toplevel` (the one without --git-common-dir). Occurrence 1
// is the context-carrying toplevel inside resolveMainRepoRootContext; occurrence
// 2 is the identity re-check's identical toplevel inside RepoFromPathContext.
// Occurrence 1 succeeds, so the wedge is reached only by the re-check — the one
// probe that used to drop the deadline.
func TestResolveProjectBindingContextBoundsAWedgedIdentityReCheck(t *testing.T) {
	root := gitRepoForProbe(t)

	// Resolve the real git BEFORE the shim goes on PATH, so the shim can
	// delegate the probes it does not wedge (same pattern as
	// installGitWithLingeringHelper).
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)

	// The wedge sleeps in the BACKGROUND with its pid recorded: CommandContext
	// kills the shim shell, never the shell's children (#3594 review).
	pidFile := filepath.Join(t.TempDir(), "wedged.pids")
	reapShimChildren(t, pidFile)
	countFile := filepath.Join(t.TempDir(), "toplevel.count")
	installFakeGit(t, fmt.Sprintf(`#!/bin/sh
# Pass every probe through to the real git except the SECOND pure
# 'rev-parse --show-toplevel' (no --git-common-dir): occurrence 1 is the
# context-carrying toplevel inside resolveMainRepoRootContext, occurrence 2 is
# the identity re-check's identical toplevel inside RepoFromPathContext.
has_toplevel=0
has_common=0
for a in "$@"; do
  [ "$a" = "--show-toplevel" ] && has_toplevel=1
  [ "$a" = "--git-common-dir" ] && has_common=1
done
if [ "$has_toplevel" = 1 ] && [ "$has_common" = 0 ]; then
  n=$(cat '%s' 2>/dev/null || echo 0)
  n=$((n + 1))
  echo "$n" > '%s'
  if [ "$n" -ge 2 ]; then
    sleep 30 &
    echo $! >> '%s'
    wait
  fi
fi
exec '%s' "$@"
`, countFile, countFile, pidFile, realGit))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type outcome struct {
		binding projectBinding
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		binding, err := resolveProjectBindingContext(ctx, root)
		done <- outcome{binding: binding, err: err}
	}()

	select {
	case got := <-done:
		require.Error(t, got.err,
			"the identity re-check's git wedge killed by the caller's deadline established "+
				"NOTHING about the binding; a clean resolution there is a verdict the probe "+
				"never earned (#3599)")
		assert.Equal(t, projectBinding{}, got.binding,
			"no binding may be committed from a probe the deadline killed")
		assert.ErrorIs(t, got.err, ErrRepoProbeUnanswered,
			"the daemon branches on this to hold the identity unknowable rather than "+
				"settle a verdict that releases the gate")
		assert.ErrorIs(t, got.err, context.DeadlineExceeded,
			"the chain must name what ended it, so a stalled mount is not read as broken "+
				"repository metadata (#3517)")
	case <-time.After(15 * time.Second):
		t.Fatal("the identity re-check must be bounded by the caller's deadline: without " +
			"the ctx threaded through RepoFromPathContext it runs under context.Background() " +
			"and a wedged git hangs the whole resolution indefinitely (#3599 restored)")
	}
}

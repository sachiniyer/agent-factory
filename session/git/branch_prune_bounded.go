package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/execgroup"
)

// Bounding the factory reset's branch-deletion path (#3096-equivalent for
// branch_prune.go).
//
// WHY THIS EXISTS SEPARATELY FROM runBoundedWorktreeGit. The reset flow already
// bounds its worktree removal (runBoundedWorktreeGit, #3096) and its session
// teardown/setup (runGitLocalCommand, #1917/#3424) under localGitTimeout.
// DeleteLocalBranch — the reset's SOLE branch-deletion path (#1736) — ran its
// `git branch -D` and the `show-ref`/`rev-parse` probes that gate it as raw
// exec.Command + Run()/CombinedOutput() with no context, no timeout, no process
// group, and no supervision. On a stalled repo filesystem (a dead NFS/FUSE
// mount, a D-state process holding a repo file) the call blocks forever,
// wedging `af reset` with no output and no bounded completion time — the exact
// wedge scenario the codebase already fixed for this same flow's worktree
// removal.
//
// So `af reset` could wedge with no bounded completion time and no message — and
// Ctrl-C, the only escape, leaves the reset half-applied. An operation that
// never returns is worse than one that fails clearly.
//
// runBoundedWorktreeGit strips inherited GIT_DIR (correct for worktree removal,
// where the -C path is the sole selector) but takes no env parameter, whereas
// DeleteLocalBranch pins GIT_DIR=<abs repoRoot> for bare-layout roots (the
// #5136 fail-closed discovery guard: without the pin, `git -C repoRoot branch
// -D` on a bare root discovers an enclosing repo and deletes there). This
// runner accepts an explicit env so the caller can reuse the existing
// env/pinning logic, and returns the raw error on non-timeout failures so the
// caller can classify on exit code and stderr (the show-ref probe distinguishes
// a silent exit 1 — determinate absence — from a loud non-zero exit — a failed
// probe — by exit code AND stderr content).
//
// A CONTEXT DEADLINE ALONE IS NOT ENOUGH — see the supervision note in
// worktree_remove_bounded.go. exec.Cmd.Wait blocks on c.Process.Wait() before
// it consults the context or WaitDelay, and waitpid does not return until the
// process exits. So against a git wedged in an uninterruptible D-state syscall
// on a dead mount, the deadline fires and Output() still blocks forever. The
// wait is therefore SUPERVISED via awaitCommandFn (the same seam
// runBoundedWorktreeGit uses): the command runs on its own goroutine and the
// caller selects against a grace period beyond the deadline. If even that
// expires, af ABANDONS the process and returns. That leaks one goroutine and
// one unreaped git — unavoidable when the kernel will not let go — and it is
// strictly better than an unresponsive CLI on a destructive path.
//
// What is guaranteed, precisely: af RETURNS and REPORTS what it could not
// delete. Not that the deletion succeeded, and not that the process died.

// ErrBranchDeletionTimedOut marks a branch-deletion operation that made no
// progress before its deadline. It is the branch-prune counterpart of
// ErrWorktreeRemovalTimedOut: a timeout leaves the deletion result UNKNOWN, so
// the caller (commands/reset.go) retains the session record and the branch in
// unverifiedBranches so a re-run of `af reset` has something to revisit — the
// same retention convention a blocked worktree or a failed existence probe
// already follows.
var ErrBranchDeletionTimedOut = errors.New("git branch operation timed out")

// branchProbeStall classifies a failed filesystem observation on the
// branch-prune path. A bounded stat that hit its deadline (or joined the latch
// an earlier deadline left) proves nothing, so it carries
// ErrBranchDeletionTimedOut, exactly as a stalled git does; any other error is
// returned as is.
func branchProbeStall(err error) error {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrBranchDeletionTimedOut) {
		return fmt.Errorf("%w: %w", ErrBranchDeletionTimedOut, err)
	}
	return err
}

// runBoundedBranchGit runs one local git command for the branch-prune path
// (DeleteLocalBranch and its existence probes) under localGitTimeout, killing
// the whole process group on the deadline and abandoning a child that cannot
// be reaped.
//
// `env` is the explicit environment the caller built (already sanitized via
// repositoryPathEnvironment and, for bare roots, with GIT_DIR pinned) — unlike
// runBoundedWorktreeGit, which builds its own env, because the branch-prune
// path's bare-layout GIT_DIR pin is caller-specific and must not be stripped.
//
// `combined` folds stderr into the returned bytes (CombinedOutput) when true;
// false uses Output, which captures stdout and leaves stderr in the returned
// *exec.ExitError's Stderr field for the caller to classify on.
//
// The raw error is returned on non-timeout failures (no commandFailureDetail
// wrapping): the show-ref probe reads exit code and stderr to distinguish
// determinate absence (exit 1, empty stderr) from a failed probe (any other
// non-zero exit or non-empty stderr), and wrapping the error would obscure
// that classification. A timeout returns ErrBranchDeletionTimedOut.
func runBoundedBranchGit(repoRoot string, env []string, combined bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localGitTimeout)
	defer cancel()

	// Resolve a relative repoRoot against the daemon's launch cwd, mirroring
	// runBoundedWorktreeGit and runGitCommandContextWithEnvironment: the daemon
	// chdirs to the AF home, so a relative `-C repoRoot` would otherwise resolve
	// beneath the AF home instead of the launch cwd.
	if !filepath.IsAbs(repoRoot) && daemonLaunchCwd != "" {
		repoRoot = filepath.Join(daemonLaunchCwd, repoRoot)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	// Start git already cwd'd at repoRoot instead of inheriting the caller's
	// cwd, for the same inherited-cwd hazard runBoundedWorktreeGit guards
	// against. Gated on repoRoot being an existing directory for the same
	// repo-gone-classification reason; the reset path's call sites pass a live
	// repo root, so the gate is a no-op there but keeps the two runners
	// consistent. BoundedLstat, not os.Stat: this runner exists to bound
	// filesystem access, and a plain Stat on a stalled FUSE/NFS root would
	// wedge the call before the context-controlled git process ever starts.
	if filepath.IsAbs(repoRoot) {
		if info, err := BoundedLstat(repoRoot); err == nil && info.IsDir() {
			cmd.Dir = repoRoot
		}
	}
	// The caller built `env` with the same sanitized environment the existence
	// probe produced its verdict under (repositoryPathEnvironment strips
	// inherited GIT_DIR, and a bare root pins GIT_DIR to the absolute repoRoot
	// so the deletion acts on the bare root and not an enclosing repo).
	cmd.Env = env
	// Own process group, so the deadline tears down git AND anything it spawned.
	execgroup.Lead(cmd)
	// Bound the post-exit wait too: a child inheriting the capture pipes would
	// otherwise block the read on pipe EOF even after the deadline killed git.
	cmd.WaitDelay = gitWaitDelay

	out, err, reaped := awaitCommandFn(cmd, combined, localGitTimeout+waitAbandonGrace)
	if !reaped {
		return nil, fmt.Errorf("%w: git %s could not be killed after %s — it is most likely wedged in an "+
			"uninterruptible syscall on a stalled filesystem, where SIGKILL stays pending and the process "+
			"cannot be reaped. af gave up waiting and left it running rather than hanging",
			ErrBranchDeletionTimedOut, strings.Join(args, " "), localGitTimeout+waitAbandonGrace)
	}
	// Reap a straggler on EVERY path, not just the timeout.
	execgroup.Reap(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited cleanly — a non-zero exit surfaces as *exec.ExitError,
		// not this — and only a straggler held the pipe past gitWaitDelay. The
		// output is already complete, so this is not a failure (#676 precedent).
		err = nil
	}
	// The err != nil guard matters: without it, the ErrWaitDelay mapping above
	// would surface as a FALSE timeout on a command that in fact succeeded
	// (#914).
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%w: git %s made no progress in %s — a stalled filesystem or a process "+
			"holding a file in the tree; git was killed rather than waited on: %w",
			ErrBranchDeletionTimedOut, strings.Join(args, " "), localGitTimeout, ctx.Err())
	}
	return out, err
}

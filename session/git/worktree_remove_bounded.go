package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Bounding the factory reset's worktree removal (#3096).
//
// WHY THIS EXISTS SEPARATELY FROM runGitLocalCommand. Cleanup() already runs the
// same three git operations under localGitTimeout, and has since #1917, which
// documented the reason: `git worktree remove -f` recursively unlinks a tree and
// blocks indefinitely on a hung network mount or a D-state process holding a file
// in it. RemoveWorktreeDir performs the identical operations and did not, because
// runGitLocalCommand is a method on *GitWorktree and the reset path holds only two
// paths. The asymmetry was the whole bug: the codebase's own hardening proved the
// stall is real, and the reset path was the one that could not use it.
//
// So `af reset` could wedge with no bounded completion time and no message — and
// Ctrl-C, the only escape, leaves the reset half-applied. An operation that never
// returns is worse than one that fails clearly.
//
// WHAT A BOUND CANNOT DO, named because it matters on the destructive path: the
// deadline works by SIGKILLing git, so it rescues stalls where git is killable. A
// process wedged in an uninterruptible (D-state) syscall against a dead mount
// ignores SIGKILL, and no in-process deadline fixes that. What the bound
// guarantees is that af RETURNS and REPORTS, not that the removal succeeds.

// ErrWorktreeRemovalTimedOut marks a worktree operation that made no progress
// before its deadline. It is deliberately reported ALONGSIDE
// ErrWorktreeStillRegistered rather than instead of it: a timeout leaves the
// registration UNKNOWN, and unknown must behave like "still registered" so the
// caller retains the session record and a re-run has something to revisit. The
// distinct sentinel exists so the message can say "stalled" rather than blaming a
// lock it never observed.
var ErrWorktreeRemovalTimedOut = errors.New("git worktree operation timed out")

// runBoundedWorktreeGit runs one local git command for the reset path under
// localGitTimeout, killing the whole process group on the deadline.
//
// combined folds stderr into the returned bytes. `worktree remove` needs that —
// mayDeleteWorktreeDir classifies on git's "validation failed:" message, which git
// writes to stderr, and a stdout-only read silently disabled the #726
// corrupted-pointer allowance once already (#2531). The porcelain probe wants
// stdout alone, so a warning on stderr cannot be parsed as a worktree entry.
func runBoundedWorktreeGit(repoRoot string, combined bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localGitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	// The environment is INHERITED rather than filtered. Cleanup's runner filters
	// because it carries a session's hook passthrough; this path has no session and
	// no secrets to scope, and a filtered env here would change what git resolves
	// (PATH, HOME, GIT_* overrides) — a behaviour change beyond the bug being fixed.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// Own process group, so the deadline tears down git AND anything it spawned.
	// exec.CommandContext's default Cancel SIGKILLs only the direct child, which on
	// a stalled unlink leaves the work still running against the dead mount.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// Bound the post-exit wait too: a child inheriting the capture pipes would
	// otherwise block the read on pipe EOF even after the deadline killed git.
	cmd.WaitDelay = gitWaitDelay

	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if cmd.Process != nil {
		// Reap a straggler on EVERY path, not just the timeout: the group is led by
		// git, which has already exited, so this is ESRCH (ignored) in the common case.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited cleanly — a non-zero exit surfaces as *exec.ExitError, not
		// this — and only a straggler held the pipe past gitWaitDelay. The output is
		// already complete, so this is not a failure (#676 precedent).
		err = nil
	}
	// The err != nil guard matters: without it, the ErrWaitDelay mapping above would
	// surface as a FALSE timeout on a command that in fact succeeded (#914).
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%w: git %s made no progress in %s — a stalled filesystem or a process "+
			"holding a file in the tree; git was killed rather than waited on: %w",
			ErrWorktreeRemovalTimedOut, strings.Join(args, " "), localGitTimeout, ctx.Err())
	}
	return out, err
}

// worktreeStalledError is the failure a reset reports for a worktree it could not
// finish removing because git stopped making progress.
//
// It wraps ErrWorktreeStillRegistered so the caller RETAINS the session record —
// a timeout leaves the registration unknown, and dropping the record on an answer
// nobody got would orphan a possibly-registered worktree and its branch with
// nothing left to plan a re-run from (#2531). The rest of the reset continues:
// commands/reset.go collects this per worktree rather than aborting, so one
// stalled mount cannot strand every other session's cleanup.
func worktreeStalledError(worktreePath string, cause error) error {
	return fmt.Errorf("%w: %w: %s could not be removed and its registration is unknown, so it was left "+
		"in place and its session record kept; check the filesystem it lives on (a hung network mount, or "+
		"a process stuck in an uninterruptible read) and re-run `af reset`",
		ErrWorktreeStillRegistered, cause, worktreePath)
}

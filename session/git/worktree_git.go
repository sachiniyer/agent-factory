package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// networkGitTimeout bounds git operations that perform network I/O (currently
// only `fetch`). A stalled remote — hung SSH/HTTP connection, dead proxy,
// unroutable host — would otherwise block the git process forever; on the
// session-creation path that hangs the daemon's RPC handler and the client
// spins on a spinner indefinitely (#896). The bound is deliberately generous so
// a slow-but-progressing fetch over a large repo still completes; it only trips
// when the fetch makes no progress at all.
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var networkGitTimeout = 60 * time.Second

// gitWaitDelay bounds how long cmd.Wait blocks after the git process exits (or
// is killed on the deadline) before the inherited stdout/stderr pipes are
// force-closed. `git fetch` spawns a transport child (ssh, git-remote-https)
// that can keep the capture pipe open past git's own exit; without a bound,
// Output() blocks on pipe EOF until that child dies — which would defeat the
// timeout above (the #856 lesson from the claude shell probe).
const gitWaitDelay = 2 * time.Second

// localGitTimeout bounds the local git commands on the session-TEARDOWN path
// (#1917). It is deliberately far larger than networkGitTimeout's sibling
// reasoning would suggest: the command it really exists for is
// `git worktree remove -f`, which recursively deletes a whole checkout —
// legitimately slow on a large tree over a cold cache, and NOT something we want
// to abort early. It only trips when removal makes no progress at all.
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var localGitTimeout = 60 * time.Second

// runGitCommand executes a local git command and returns any error.
// Only stdout is returned on success so callers parsing the output (e.g. SHAs
// or porcelain status) are not corrupted by warnings git emits on stderr.
// On error, stderr is folded into the returned error for diagnostics.
//
// It runs with a background context, so it is UNBOUNDED. This used to be
// justified as "local-only operations cannot stall the way a fetch can", which
// is false — see runGitLocalCommand, which bounds the teardown-path callers that
// disproved it. The remaining callers here are pure metadata reads (rev-parse,
// show-ref, merge-base) plus `worktree add`, which stay unbounded on purpose:
// `worktree add` runs the repo's post-checkout hook, i.e. arbitrary user code
// whose legitimate runtime (a big install step) has no defensible upper bound,
// and it is on the session-CREATE path, where a stall is visible and cancellable
// rather than wedging a kill forever.
//
// Network operations must use runGitNetworkCommand instead (#896); local
// operations on a teardown path must use runGitLocalCommand (#1917).
func (g *GitWorktree) runGitCommand(path string, args ...string) (string, error) {
	return g.runGitCommandContext(context.Background(), path, args...)
}

// runGitLocalCommand runs a local git command under localGitTimeout, so a step
// that makes no progress surfaces an actionable timeout error instead of hanging
// the caller forever (#1917). Like runGitNetworkCommand, the underlying process
// group is SIGKILLed on the deadline.
//
// This exists because the "local git cannot stall" assumption above does not
// survive contact with teardown: `git worktree remove -f` recursively unlinks a
// tree and blocks indefinitely on a hung network mount or a D-state process
// holding a file in it. Unbounded, that stalls GitWorktree.Cleanup, which stalls
// the daemon's KillSession while it holds a per-session kills-in-flight guard —
// and the session becomes permanently undeletable for the daemon's lifetime.
//
// Bounding is a real improvement but not a total one, and the limit is worth
// naming: the deadline works by SIGKILLing git, so it only rescues stalls where
// git is actually killable. A process wedged in an uninterruptible (D-state)
// syscall against a dead mount ignores SIGKILL, and no in-process deadline can
// fix that.
func (g *GitWorktree) runGitLocalCommand(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localGitTimeout)
	defer cancel()
	output, err := g.runGitCommandContext(ctx, path, args...)
	// Only treat a tripped deadline as a timeout when the command actually
	// failed — runGitCommandContext maps a bare exec.ErrWaitDelay to nil, and
	// without the err != nil guard that race would surface as a false timeout on
	// a command that in fact succeeded (#914, same guard as runGitNetworkCommand).
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output, fmt.Errorf("git %s timed out after %s (no progress; a stalled filesystem or an unkillable process holding the worktree): %w",
			strings.Join(args, " "), localGitTimeout, ctx.Err())
	}
	return output, err
}

// runGitNetworkCommand runs a git command that performs network I/O under
// networkGitTimeout, so a stalled remote returns an actionable timeout error
// instead of hanging the caller forever (#896). The underlying process group is
// SIGKILLed on the deadline so the transport child (ssh / git-remote-https) is
// torn down with git rather than orphaned while still holding the connection.
func (g *GitWorktree) runGitNetworkCommand(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), networkGitTimeout)
	defer cancel()
	output, err := g.runGitCommandContext(ctx, path, args...)
	// Only treat a tripped deadline as a timeout when the command actually
	// failed. runGitCommandContext converts an exec.ErrWaitDelay (git itself
	// exited successfully, only a transport child held the capture pipe open
	// past the deadline) into err == nil; without the err != nil guard that
	// race would surface as a false timeout even though the fetch succeeded
	// (#914). github.go's FetchPRInfo guards the same way.
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output, fmt.Errorf("git %s timed out after %s (remote unreachable or stalled): %w",
			strings.Join(args, " "), networkGitTimeout, ctx.Err())
	}
	return output, err
}

// runGitCommandContext is the shared implementation behind runGitCommand and
// runGitNetworkCommand. It runs git in its own process group so the whole
// command tree — git plus any transport child it spawns — can be torn down
// together when ctx is cancelled; GIT_TERMINAL_PROMPT=0 stops git from blocking
// on an interactive credential/passphrase prompt (another way a fetch hangs
// forever under the daemon, which has no terminal attached).
func (g *GitWorktree) runGitCommandContext(ctx context.Context, path string, args ...string) (string, error) {
	baseArgs := []string{"-C", path}
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	// Fail fast instead of blocking on a credential/passphrase prompt when a
	// remote needs auth and no terminal is attached.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	// Own process group so the deadline kills git AND its transport child
	// together. exec.CommandContext's default Cancel SIGKILLs only the git
	// process, leaving ssh / git-remote-https orphaned and still holding the
	// network connection (mirrors config.go's claude probe and the hook
	// runner, #610/#769/#856).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid targets the whole group led by cmd.Process.Pid. A group
		// already gone (ESRCH) maps to os.ErrProcessDone, which Wait ignores
		// rather than reporting as a command failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// Bound the post-exit wait: a transport child can inherit the capture pipes
	// and, without a bound, Output() would block on pipe EOF until that child
	// exits even after the deadline killed git (#856).
	cmd.WaitDelay = gitWaitDelay

	output, err := cmd.Output()
	if cmd.Process != nil {
		// Reap any transport child that outlived git on every exit path —
		// normal completion or timeout — so a fetch never leaks a
		// connection-holding process (#769 pattern). The group is led by git,
		// which has already exited, so this is ESRCH (ignored) in the common
		// case.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited (successfully — a non-zero exit would surface as an
		// ExitError, not ErrWaitDelay); only a transport child held the capture
		// pipe open past gitWaitDelay and was just killed above. The command's
		// output is already complete, so this is not a failure (#676 precedent).
		err = nil
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(output), fmt.Errorf("git command failed: %s (%w)", string(exitErr.Stderr), err)
		}
		return string(output), fmt.Errorf("git command failed: %w", err)
	}

	return string(output), nil
}

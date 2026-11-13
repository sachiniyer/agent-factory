package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/execgroup"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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

// localGitTimeout bounds the destructive local git commands on the session
// teardown path (#1917) and, since #3424, the identical ones on the session
// SETUP path. It is deliberately far larger than networkGitTimeout's sibling
// reasoning would suggest: the command it really exists for is
// `git worktree remove -f`, which recursively deletes a whole checkout —
// legitimately slow on a large tree over a cold cache, and NOT something we want
// to abort early. It only trips when removal makes no progress at all.
//
// A var (not a const) only so tests can shorten it; production never reassigns.
var localGitTimeout = 60 * time.Second

// SetLocalGitTimeoutForTest shortens localGitTimeout and returns a restore func,
// so a test OUTSIDE this package can drive a tripped teardown deadline in
// milliseconds instead of the production 60s. Package-internal tests assign the
// var directly (see shortenLocalTimeout); this exists for session/, whose
// archive teardown has to classify a cut-off relocate (git.ErrRelocateStateUnknown)
// and can only produce one by actually tripping this bound. Test-only.
func SetLocalGitTimeoutForTest(d time.Duration) func() {
	prev := localGitTimeout
	localGitTimeout = d
	return func() { localGitTimeout = prev }
}

// daemonLaunchCwd is the cwd the daemon process held BEFORE chdirToNeutralHome
// moved it onto the AF home. The daemon records it once at startup (via
// SetDaemonLaunchCwd) so the git runners can resolve a relative `-C path`
// against the launch cwd rather than the post-chdir AF home. Without it a
// restored session whose persisted path NewGitWorktreeFromStorage stored
// verbatim (a relative repo or worktree path) would resolve beneath the AF
// home and fail every git operation — the regression the chdir introduced for
// the relative-path edge case. Empty in tests and any non-daemon caller, which
// leaves a relative path on its pre-chdir behaviour (resolved against the
// current cwd).
var daemonLaunchCwd string

// SetDaemonLaunchCwd records the daemon's pre-chdir cwd so the git runners can
// resolve relative paths against it after chdirToNeutralHome moves the daemon
// onto the AF home. Called once from the daemon before that chdir; never after.
func SetDaemonLaunchCwd(dir string) {
	if dir != "" {
		daemonLaunchCwd = dir
	}
}

// runGitCommand executes a local git command and returns any error.
// Only stdout is returned on success so callers parsing the output (e.g. SHAs
// or porcelain status) are not corrupted by warnings git emits on stderr.
// On error, stderr is folded into the returned error for diagnostics.
//
// It runs with a background context, so it is UNBOUNDED. This used to be
// justified as "local-only operations cannot stall the way a fetch can", which
// is false — see runGitLocalCommand, which bounds the teardown-path callers that
// disproved it. Most callers left here are metadata reads (rev-parse, show-ref,
// merge-base, for-each-ref, status); the writes that remain are `worktree add`,
// the archive snapshot's `add -A` + `commit` (worktree_push.go) and the rename's
// `branch -m` (branch_rename.go). None of them recursively unlinks a checkout,
// and `worktree add` in particular stays unbounded on purpose: it runs the
// repo's post-checkout hook, i.e. arbitrary user code whose legitimate runtime
// (a big install step) has no defensible upper bound.
//
// The setup path used to be here too, and that was #3424: `worktree remove -f`,
// `worktree prune` and `branch -D` ran through this unbounded runner on session
// CREATE while the identical teardown calls had been bounded since #1917. Those
// callers now use runGitLocalCommand — see worktree_setup_bounded.go.
//
// Network operations must use runGitNetworkCommand instead (#896); a local
// operation that DESTROYS something — a checkout, a registration, a branch —
// must use runGitLocalCommand, on the teardown path (#1917) and on the setup
// path alike (#3424).
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
	// A timeout is the deadline ending git while it ran: errEndedByContext,
	// which the runner sets only when os/exec called Cancel. Not ctx.Err(),
	// which is also true when git failed on its own just before the deadline and
	// WaitDelay or the straggler reap ran past it, and which would hide git's
	// own failure behind a timeout (#7243). A command that succeeded is never a
	// timeout either (#914). Both errors stay in the chain: callers test for
	// context.DeadlineExceeded, and git's stderr rides on its own error.
	if errors.Is(err, errEndedByContext) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output, fmt.Errorf("git %s timed out after %s (no progress; a stalled filesystem or an unkillable process holding the worktree): %w: %w",
			strings.Join(args, " "), localGitTimeout, ctx.Err(), err)
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
	// A timeout is the deadline ending git while it ran (see runGitLocalCommand).
	// A fetch that failed on its own, say on authentication, just before the
	// deadline, with a transport child holding stderr past gitWaitDelay, is
	// that failure and keeps git's stderr, not "remote unreachable or stalled"
	// (#7243). A fetch that succeeded is never a timeout (#914).
	if errors.Is(err, errEndedByContext) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output, fmt.Errorf("git %s timed out after %s (remote unreachable or stalled): %w: %w",
			strings.Join(args, " "), networkGitTimeout, ctx.Err(), err)
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
	return g.runGitCommandContextWithEnvironment(ctx, path, g.gitCommandEnvironment(), args...)
}

func (g *GitWorktree) runGitCommandContextWithEnvironment(
	ctx context.Context,
	path string,
	environment []string,
	args ...string,
) (string, error) {
	// Resolve a relative path against the daemon's launch cwd (captured before
	// chdirToNeutralHome moved the daemon onto the AF home) so a restored session
	// whose persisted path NewGitWorktreeFromStorage stored verbatim still targets
	// the same repo it did before the chdir. Without this a relative `-C path`
	// would resolve beneath the AF home the daemon now runs in instead of the
	// spawner's cwd, breaking every git operation for that edge case. The path is
	// now absolute, so the existing-directory gate below sets cmd.Dir and removes
	// the inherited-cwd window for relative paths too. Empty daemonLaunchCwd
	// (tests, any non-daemon caller) leaves a relative path untouched so it
	// resolves against the current cwd as before.
	if !filepath.IsAbs(path) && daemonLaunchCwd != "" {
		path = filepath.Join(daemonLaunchCwd, path)
	}
	baseArgs := []string{"-C", path}
	cmd := exec.CommandContext(ctx, "git", append(baseArgs, args...)...)
	// Start git already cwd'd at `path` rather than inheriting the daemon's cwd.
	// The daemon can be auto-started from a managed worktree and never chdirs, so
	// without cmd.Dir the git child shows that inherited worktree as
	// /proc/<pid>/cwd during git's startup window (before `git -C path` takes
	// effect). reapWorktreeWriters selects by cwd and would then SIGTERM an
	// unrelated session's git command during a concurrent reap of the inherited
	// worktree. Setting cmd.Dir removes the false positive at its source; `-C
	// path` stays as the repo selector (and is the sole selector, since
	// repositoryPathEnvironment strips GIT_DIR etc. below). hooks.go sets
	// cmd.Dir = run.worktreePath for the same reason — the package convention,
	// now followed here.
	//
	// Gated on path being an existing directory: the repo-gone origin probe
	// (worktree_repo_gone_authorization.go) deliberately runs `git -C <gone-or-
	// non-dir path>` to classify a missing origin from git's *exec.ExitError. If
	// cmd.Dir were set to a gone path, exec would fail to start git at all
	// (fork/exec ENOENT/ENOTDIR) and the classifier would see an os.PathError
	// instead of the ExitError it keys on. The existing-directory case — every
	// ordinary worktree/repo operation, and the only case the inherited-cwd
	// false positive can bite — still gets cmd.Dir; the gone-path probe keeps
	// its pre-fix behaviour (a brief inherited-cwd window against a command
	// git fails immediately, whose worst case is a SIGTERM'd probe that
	// classifies as the fail-closed "unknown", not a deletion authorization).
	//
	// A relative path is resolved against the daemon's launch cwd above, so by
	// here it is absolute; the existing-directory gate then sets cmd.Dir too,
	// closing the inherited-cwd window for relative paths as well. Production
	// worktree/repo paths are always absolute (every constructor normalizes via
	// filepath.Abs or git rev-parse --show-toplevel), so this only matters for a
	// hand-edited or externally-authored relative path in storage — which now
	// resolves against the launch cwd rather than the post-chdir AF home.
	if filepath.IsAbs(path) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			cmd.Dir = path
		}
	}
	// Fail fast instead of blocking on a credential/passphrase prompt when a
	// remote needs auth and no terminal is attached. Force stable diagnostics so
	// repository classification is fail-closed and locale-independent.
	cmd.Env = repositoryPathEnvironment(environment)
	// Own process group so the deadline kills git AND its transport child
	// together. exec.CommandContext's default Cancel SIGKILLs only the git
	// process, leaving ssh / git-remote-https orphaned and still holding the
	// network connection (mirrors config.go's claude probe and the hook
	// runner, #610/#769/#856).
	execgroup.Lead(cmd)
	canceled := execgroup.WatchCancel(cmd)
	// Bound the post-exit wait: a transport child can inherit the capture pipes
	// and, without a bound, Output() would block on pipe EOF until that child
	// exits even after the deadline killed git (#856).
	cmd.WaitDelay = gitWaitDelay

	// execgroup.Output, not cmd.Output plus a group Reap. It still kills a
	// transport child that held the capture pipe past gitWaitDelay after git
	// exited, on every exit path, so a fetch never leaks a connection-holding
	// process (#769 pattern). What it no longer kills is a member of git's group
	// that holds none of git's pipes. Under ControlMaster/ControlPersist that is
	// the master's ProxyCommand (or ProxyJump's `ssh -W`), which OpenSSH leaves
	// in this group as the master's transport while the master itself detaches:
	// a group reap after a successful fetch killed it, and the master with it,
	// so every later fetch re-authenticated (#7121). A deadline still kills the
	// whole group: KillOnCancel's sweep, then a second once git has been waited,
	// keyed on the Cancel having run, which is what Reap gave it before (#7241).
	output, err := execgroup.Output(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited (successfully — a non-zero exit would surface as an
		// ExitError, not ErrWaitDelay); only a transport child held the capture
		// pipe open past gitWaitDelay and was just killed. The command's output
		// is already complete, so this is not a failure (#676 precedent).
		err = nil
	}
	if err != nil {
		if canceled() {
			err = endedByContext{err}
		}
		if detail := commandFailureDetail(err); detail != "" {
			return string(output), fmt.Errorf("git command failed: %s (%w)", detail, err)
		}
		return string(output), fmt.Errorf("git command failed: %w", err)
	}

	return string(output), nil
}

// errEndedByContext marks a git failure its context caused: os/exec called
// Cancel, so the context ended git while it still ran (execgroup.WatchCancel).
// It is what "timed out" means, where ctx.Err() alone is also true for a git
// that failed on its own while cleanup ran past the deadline.
var errEndedByContext = errors.New("git was ended by its context")

// endedByContext carries errEndedByContext beside git's own error without
// changing its text, so commandFailureDetail still finds the *exec.ExitError.
type endedByContext struct{ err error }

func (e endedByContext) Error() string   { return e.err.Error() }
func (e endedByContext) Unwrap() []error { return []error{e.err, errEndedByContext} }

func (g *GitWorktree) gitCommandEnvironment() []string {
	return append(sessionenv.Filter(os.Environ(), "", g.HookEnvironment()),
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

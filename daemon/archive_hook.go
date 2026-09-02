package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// onArchiveHookTimeout bounds an operator hook that never returns. Archive is a
// synchronous lifecycle operation, but real dependency-tree sweeps can take
// minutes on a large worktree, so this is intentionally generous.
//
// A var (not a const) only so tests can shorten it — the deadline's interaction
// with WaitDelay cleanup is exactly what #3407 got wrong, and it is not testable
// on a 30-minute clock. Production never reassigns it.
var onArchiveHookTimeout = 30 * time.Minute

const onArchiveHookWaitDelay = 2 * time.Second

// A hook is operator-authored and may be arbitrarily noisy. Keep only the tail:
// the end normally contains the actionable failure while a hard ceiling prevents
// a 30-minute hook from growing the daemon heap without bound.
const onArchiveHookOutputLimit = 64 * 1024

type archiveHookOutputTail struct {
	data      []byte
	truncated bool
}

func (w *archiveHookOutputTail) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) >= onArchiveHookOutputLimit {
		w.data = append(w.data[:0], p[len(p)-onArchiveHookOutputLimit:]...)
		w.truncated = true
		return written, nil
	}
	if overflow := len(w.data) + len(p) - onArchiveHookOutputLimit; overflow > 0 {
		copy(w.data, w.data[overflow:])
		w.data = w.data[:len(w.data)-overflow]
		w.truncated = true
	}
	w.data = append(w.data, p...)
	return written, nil
}

func (w *archiveHookOutputTail) String() string {
	if !w.truncated {
		return string(w.data)
	}
	return fmt.Sprintf("[output truncated to last %d bytes]\n%s", onArchiveHookOutputLimit, w.data)
}

type onArchiveHookContext struct {
	sessionID   string
	title       string
	repoRoot    string
	worktree    string
	archivePath string
}

// runOnArchiveHook resolves the command at operation time and runs it at the
// teardown chokepoint. ResolveConfig is load-bearing security policy here: the
// manifest admits on_archive_command from global and personal-project config,
// while LoadInRepoConfig rejects the same key before any repository-controlled
// command can execute.
func runOnArchiveHook(hookCtx onArchiveHookContext) error {
	// BEFORE anything else, and before the caller's move: a previous daemon
	// generation may have died with this session's post-worktree or archive hook
	// still running in its scope — the survival the unbound shape now guarantees
	// (#3650). Archive is retryable and it MOVES the tree, so this is the #2770
	// ordering restated across the restart boundary: running the operator's
	// command a second time while the first is still writing through its old cwd
	// is exactly what that ordering exists to prevent. It runs even when no hook
	// is configured, because the MOVE is the hazard, not just the second run.
	//
	// It needs no persisted handle: the prefix is derived from the session id, so
	// it is correct for a session whose record never carried one — including one
	// with no post_worktree_commands at all, which is the case with no other
	// handle available. RunningDaemonProcess() is the gate for the same reason it
	// is in session/git: only that process creates these scopes, and only it is
	// guaranteed a reachable user manager.
	scopePrefix := ""
	if systemdunit.RunningDaemonProcess() {
		scopePrefix = systemdunit.HookScopeUnitPrefix(hookCtx.sessionID)
		if err := systemdunit.StopHookScopes(scopePrefix); err != nil {
			return fmt.Errorf(
				"%w: refusing to run the on-archive hook for %s because a previous run's hook scopes (%s-*) could not be stopped: %w",
				session.ErrHookTeardownUnconfirmed, hookCtx.sessionID, scopePrefix, err)
		}
	}

	// Prefer the live worktree so a bare repository's identity directory is
	// never treated as a checkout. If Git metadata is already unavailable, keep
	// the historical identity-root fallback: on_archive_command is admitted only
	// from operator config and can still be useful while salvaging a lost repo.
	repo, repoErr := config.RepoFromPath(hookCtx.worktree)
	var resolved *config.ResolvedConfig
	var err error
	if repoErr == nil {
		resolved, err = config.ResolveConfigForRepo(repo)
	} else {
		resolved, err = config.ResolveConfig(hookCtx.repoRoot)
	}
	if err != nil {
		return fmt.Errorf("load on-archive hook configuration: %w", err)
	}
	command := strings.TrimSpace(resolved.OnArchiveCommand)
	if command == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), onArchiveHookTimeout)
	defer cancel()

	var output archiveHookOutputTail
	// Same lifetime class as post_worktree_commands, same shape (#3650): a
	// daemon-spawned operator hook enters its own transient scope with no
	// dependency edge to the daemon unit, so a dependency sweep is charged to its
	// own cgroup instead of the daemon's and is not killed by a restart or an
	// auto-upgrade. The scope shares the session's af-hook-<id> prefix, so the
	// worktree sweep that runs before a rebuild or removal names it too.
	scopeUnit := systemdunit.HookScopeUnit(scopePrefix, systemdunit.NewHookScopeGeneration(), 0)
	program, argv := systemdunit.UnboundScopeArgv(scopeUnit, "sh", "-c", command)
	cmd := exec.CommandContext(ctx, program, argv...)
	cmd.Dir = hookCtx.worktree
	cmd.Env = append(
		sessionenv.Filter(os.Environ(), "", resolved.SessionEnvPassthrough),
		"AF_SESSION_ID="+hookCtx.sessionID,
		"AF_SESSION_TITLE="+hookCtx.title,
		"AF_REPO_ROOT="+hookCtx.repoRoot,
		"AF_WORKTREE_PATH="+hookCtx.worktree,
		"AF_ARCHIVE_PATH="+hookCtx.archivePath,
	)
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = onArchiveHookWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}

	err = cmd.Run()
	if cmd.Process != nil {
		// A hook may background a descendant and exit. Kill the process group on
		// every path so no cleanup process survives the archive operation with a
		// cwd or inherited pipe in the worktree. systemd-run --scope execs rather
		// than forks, so cmd.Process.Pid is still the hook shell and this signals
		// exactly the group it always did.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// The scope is the wider net over the same tree: a descendant that left the
	// process group (setsid) is still in the control group, where the SIGKILL
	// above cannot reach it. `systemctl stop` waits for the job — StopScopeUnits
	// passes no --no-block — so a failure here is not "the stop is still running",
	// it is no proof the stop completed at all.
	//
	// That makes it a SAFETY error, not a hook error: the caller relocates the
	// worktree immediately after this returns, and every ordinary hook failure is
	// deliberately best-effort there. Wrapping it in ErrHookTeardownUnconfirmed is
	// what makes the move refuse instead (#3650 review). It outranks the hook's
	// own outcome below, which is only a report.
	if scopeUnit != "" {
		if stopErr := systemdunit.StopScopeUnits(scopeUnit); stopErr != nil {
			log.WarningLog.Printf("on-archive hook scope %s did not stop: %v", scopeUnit, stopErr)
			return fmt.Errorf(
				"%w: the on-archive hook's scope %s could not be stopped, so a descendant may still be writing into the worktree: %w",
				session.ErrHookTeardownUnconfirmed, scopeUnit, stopErr)
		}
	}
	// What the hook ITSELF did outranks what the clock says (#3407). Both checks
	// below used to run in the opposite order, so a deadline that fired anywhere in
	// this window reported a timeout for a hook that had already finished — and an
	// archive hook is a committed operation whose outcome an operator reads out of
	// this one message.
	if errors.Is(err, exec.ErrWaitDelay) {
		// The shell itself succeeded — a non-zero exit surfaces as an *exec.ExitError,
		// never as this — and WaitDelay only found an inherited process still holding
		// the capture pipe, which the process-group kill above reaped. Match the
		// existing post-worktree hook contract.
		//
		// This is checked BEFORE ctx.Err() because both are routinely true together:
		// a hook that exits in milliseconds after backgrounding a child keeps Run
		// blocked on that child's pipe for the full WaitDelay, and any deadline
		// shorter than that elapses inside the wait. The hook had already succeeded;
		// only the cleanup crossed the line.
		return nil
	}
	// A deadline that fires while the hook is still RUNNING is a timeout. One that
	// fires after the hook reached its own exit is not — the hook ran, and its exit
	// status is the outcome to report, straggler or no straggler. ProcessState is
	// what tells them apart: Exited() is true only when the shell terminated on its
	// own terms, and false when the deadline's SIGKILL ended it (Run reports
	// "signal: killed" and the exit code is -1). Without that second clause a hook
	// that exited 23 while a child held the pipe past the deadline was reported as
	// "timed out after 1s" — a failure the operator cannot act on, standing in for a
	// specific exit status and its output, which they can.
	if ctx.Err() != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Exited()) {
		return fmt.Errorf("timed out after %s%s", onArchiveHookTimeout, archiveHookOutput(output.String()))
	}
	if err != nil {
		return fmt.Errorf("%w%s", err, archiveHookOutput(output.String()))
	}
	return nil
}

func archiveHookOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	return ": " + output
}

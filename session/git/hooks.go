package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// hookRun is one post-worktree hook run's inputs. ScopeSessionID is empty for
// every TUI- and CLI-initiated worktree, which is what keeps that path
// byte-for-byte as it was: no scope name is derived, no systemd-run is spawned,
// and no durable handle is recorded.
type hookRun struct {
	// leaseProgress protects a newly published list until its local runner exits.
	leaseProgress bool
	progress      *hookProgress
	repoPath      string
	worktreePath  string
	passthrough   []string
	// scopeSessionID is the session identity a daemon-owned scope is named
	// after. Only the daemon's own spawn is relocated (see RunningDaemonProcess
	// below); this merely says which name to use when it is.
	scopeSessionID string
	// onScopeLaunched is invoked with the scope prefix the first time a command
	// actually enters a scope, so the session can persist the durable handle a
	// later daemon generation needs to find a survivor.
	onScopeLaunched func(prefix string)
	// onProgressPublished lets the daemon create path retain the runner lease
	// until its owner row is durably committed or the create aborts.
	onProgressPublished func(*hookProgress)
}

// RunPostWorktreeHooksAsyncWithEnvironment runs the per-repo post_worktree_commands
// in the background. Each command is executed sequentially via "sh -c" with the
// working directory set to worktreePath. The provided context can be used to
// cancel in-flight hooks (e.g. when the worktree is being cleaned up). Each
// command runs as the leader of its own process group so that the whole tree
// — including grandchildren the script backgrounded with `&` or `disown` —
// is killed together once the hook's shell exits, whether that exit is normal
// completion or cancellation. Backgrounded grandchildren therefore never
// outlive their parent hook.
// Errors are logged but do not propagate.
//
// Repository-provided commands receive common Git/runtime names plus only the
// operator's explicit extensions. Selecting an agent for the session does not
// grant that agent's provider credentials to repository code.
//
// The returned channel is closed once every hook has finished — whether by
// normal completion, failure, or ctx cancellation. It is closed immediately
// when there are no hooks to run (or the repo config can't be resolved). It
// remains open when durable storage cannot prove a launch terminal, because
// later entries are still owed. It lets callers tell whether provisioning is
// still in flight; in particular the readiness wait uses it so a slow build
// hook running concurrently with the agent is not charged against the agent's
// startup budget (see task.WaitForReady).
func RunPostWorktreeHooksAsyncWithEnvironment(ctx context.Context, repoPath, worktreePath string, passthrough []string) <-chan struct{} {
	return runPostWorktreeHooks(ctx, hookRun{
		repoPath:     repoPath,
		worktreePath: worktreePath,
		passthrough:  passthrough,
	})
}

func runPostWorktreeHooks(ctx context.Context, run hookRun) <-chan struct{} {
	done := make(chan struct{})
	// A bare repository's identity path contains no checked-out config. Once a
	// worktree has been provisioned, resolve through it to keep identity-keyed
	// legacy config on the bare directory and checked-in config on the checkout.
	// Retain the raw-path fallback for recovery and callers whose recorded repo is
	// temporarily unavailable.
	var cmds []string
	if run.progress != nil {
		cmds = run.progress.Commands
	} else {
		repo, repoErr := config.RepoFromPath(run.worktreePath)
		var repoCfg *config.ResolvedConfig
		var err error
		if repoErr == nil {
			repoCfg, err = config.ResolveConfigForRepo(repo)
		} else {
			repoCfg, err = config.ResolveConfig(run.repoPath)
		}
		if err != nil {
			log.WarningLog.Printf("failed to resolve repo config for hooks: %v", err)
			close(done)
			return done
		}
		if len(repoCfg.PostWorktreeCommands) == 0 {
			close(done)
			return done
		}

		cmds = repoCfg.PostWorktreeCommands
	}
	// Derived once per RUN, so every command of this run shares a generation and
	// the recorded prefix names all of them. Being the process systemd started is
	// the WHOLE gate: a TUI or CLI that creates the worktree itself spawns exactly
	// what it spawns today.
	scopePrefix := ""
	if systemdunit.RunningDaemonProcess() {
		// And a missing session id must not quietly put the hook back in the
		// daemon's cgroup, so the worktree path stands in as the identity — stable
		// for the same tree, and still a durable handle because the prefix that is
		// used is the prefix that gets recorded. Without this, any future call path
		// that forgets to plumb the id fails open into exactly the defect #3650
		// fixes, and nothing would say so.
		identity := run.scopeSessionID
		if strings.TrimSpace(identity) == "" {
			identity = worktreePathScopeIdentity(run.worktreePath)
		}
		scopePrefix = systemdunit.HookScopeUnitPrefix(identity)
	}
	generation := systemdunit.NewHookScopeGeneration()
	if run.progress != nil {
		scopePrefix = run.progress.Prefix
		generation = run.progress.Generation
	} else if scopePrefix != "" {
		var progressErr error
		run.leaseProgress = true
		run.progress, progressErr = newHookProgress(run, cmds, scopePrefix, generation)
		if progressErr != nil {
			log.ErrorLog.Printf("post-worktree hooks not started: persist list: %v", progressErr)
			close(done)
			return done
		}
		if run.onProgressPublished != nil {
			run.onProgressPublished(run.progress)
		}
	}
	go func() {
		journalTerminal := false
		defer close(done)
		if run.progress != nil {
			defer func() {
				if journalTerminal {
					if err := run.progress.markFinished(); err != nil {
						log.ErrorLog.Printf("cannot record post-worktree hook completion: %v", err)
					}
				}
				run.progress.releaseLease()
			}()
		}
		scopeRecorded := false
		for index := 0; index < len(cmds); {
			cmdStr := cmds[index]
			if run.progress != nil {
				state, err := run.progress.entryState(index)
				if err != nil {
					log.WarningLog.Printf("cannot determine post-worktree hook entry %d state: %v; waiting to recover the ordered suffix", index, err)
					if waitForHookEntryRecovery(ctx, run.progress, index) {
						continue
					}
					return
				}
				if state == hookEntryFinished {
					index++
					continue
				}
				if state == hookEntryStarted {
					if waitForHookEntryRecovery(ctx, run.progress, index) {
						continue
					}
					return
				}
			}
			select {
			case <-ctx.Done():
				journalTerminal = true
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", run.worktreePath)
				return
			default:
			}
			outputFile, outputErr := openHookLog(hooklog.PostWorktree)
			if outputErr != nil {
				log.ErrorLog.Printf("post-worktree hook %q was not started: create daemon-independent output log: %v", cmdStr, outputErr)
				if !run.progress.recordLaunchFailure(ctx, index, outputErr) {
					if waitForHookEntryRecovery(ctx, run.progress, index) {
						continue
					}
					return
				}
				index++
				continue
			}
			outputPath := outputFile.Name()
			log.InfoLog.Printf("running post-worktree hook in %s (output: %s): %s", run.worktreePath, outputPath, cmdStr)

			// The daemon-spawned hook enters a transient scope with NO edge to the
			// daemon unit, so the operator's build is charged to its own cgroup and
			// survives a daemon restart or auto-upgrade (#3650). Its stdout and stderr
			// are the *os.File above rather than an in-memory writer: os/exec therefore
			// gives the child the descriptor directly, with no capture-pipe reader in
			// the daemon whose exit could SIGPIPE the surviving hook (#4010).
			// systemd-run --scope EXECs the command rather than forking it — measured:
			// the pid the caller started is the pid of the hook shell — so Setpgid
			// below still makes the shell its own process-group leader and the group
			// teardown further down keeps meaning exactly what it meant before. The
			// scope is a strictly wider net over the same tree, not a replacement.
			scopeUnit := systemdunit.HookScopeUnit(scopePrefix, generation, index)
			cmd := exec.Command("sh", "-c", cmdStr)
			if scopeUnit != "" {
				cmd = systemdunit.NewUnboundScopeCommand(scopeUnit, "sh", run.progress.command(index, cmdStr)...)
			}
			cmd.Env = sessionenv.Filter(os.Environ(), "", run.passthrough)
			cmd.Dir = run.worktreePath
			cmd.Stdout = outputFile
			cmd.Stderr = outputFile
			// Place sh in its own process group so we can signal the whole
			// tree on cancellation. exec.CommandContext only kills the
			// immediate shell, leaving grandchildren the script backgrounded
			// with `&` or `disown` alive — they get reparented to init and
			// outlive the session (see #610).
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				_ = outputFile.Close()
				log.ErrorLog.Printf("post-worktree hook %q failed to start (full output: %s): %v", cmdStr, outputPath, err)
				if !run.progress.recordLaunchFailure(ctx, index, err) {
					if waitForHookEntryRecovery(ctx, run.progress, index) {
						continue
					}
					return
				}
				index++
				continue
			}
			// Record the durable handle as soon as one scope exists, not when the
			// run ends: a daemon that dies mid-build is exactly the case the handle
			// is for, and a handle written only on completion would never be written
			// in it.
			if scopeUnit != "" && !scopeRecorded {
				scopeRecorded = true
				if run.onScopeLaunched != nil {
					run.onScopeLaunched(scopePrefix)
				}
			}

			// While the hook is running, a watchdog SIGKILLs the whole process
			// group on cancellation (negative pid targets the group led by
			// cmd.Process.Pid) so a long-running hook is torn down promptly.
			// doneCh stops the watchdog once cmd.Wait() returns so it does not
			// leak across loop iterations.
			doneCh := make(chan struct{})
			go func(pgid int) {
				select {
				case <-ctx.Done():
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				case <-doneCh:
				}
			}(cmd.Process.Pid)

			waitErr := cmd.Wait()
			close(doneCh)

			// Always SIGKILL the process group once the shell has exited, not
			// just on cancellation. If the script backgrounded a process with
			// `&` or `disown` and the shell exited immediately (no `wait`), that
			// grandchild keeps running in the worktree's process group; the
			// watchdog above only fires on cancellation and may already have
			// exited via doneCh. Signalling the group here reaps any survivors
			// on every exit path — normal completion or a cancellation that
			// raced ahead of doneCh — so no grandchild outlives its parent hook
			// (see #610, #769).
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			// And close the scope behind the same exit, synchronously, so that
			// hooksDone closing keeps meaning "nothing from this run is left" for
			// the scope too — a grandchild that escaped the process group (a
			// setsid'd child) is still inside the scope's control group. Stopping
			// an already-collected scope is a no-op.
			var scopeStopErr error
			if scopeUnit != "" {
				if err := stopHookScopeUnits(scopeUnit); err != nil {
					scopeStopErr = err
					log.WarningLog.Printf("post-worktree hook scope %s did not stop (full output: %s): %v", scopeUnit, outputPath, err)
				}
			}
			if run.progress != nil {
				state, stateErr := run.progress.entryState(index)
				if stateErr != nil {
					log.WarningLog.Printf("cannot verify post-worktree hook entry %d completion: %v; waiting to recover the ordered suffix", index, stateErr)
					_ = outputFile.Close()
					if waitForHookEntryRecovery(ctx, run.progress, index) {
						continue
					}
					return
				}
				if state == hookEntryStarted {
					if !run.progress.terminalizeInactiveClaim(ctx, index, waitErr) {
						log.WarningLog.Printf("post-worktree hook entry %d has no terminal outcome; waiting to recover the ordered suffix", index)
						_ = outputFile.Close()
						if waitForHookEntryRecovery(ctx, run.progress, index) {
							continue
						}
						return
					}
				}
				if state == hookEntryUnclaimed {
					if waitErr == nil {
						waitErr = fmt.Errorf("hook launcher exited before claiming entry %d", index)
					}
					if scopeStopErr != nil || !run.progress.recordLaunchFailure(ctx, index, waitErr) {
						_ = outputFile.Close()
						if waitForHookEntryRecovery(ctx, run.progress, index) {
							continue
						}
						return
					}
				}
			}
			outputTail, outputReadErr := hooklog.CloseAndReadTail(outputFile)
			if outputReadErr != nil {
				log.WarningLog.Printf("post-worktree hook %q output tail could not be read from %s: %v", cmdStr, outputPath, outputReadErr)
			}

			if ctx.Err() != nil {
				journalTerminal = true
				if scopeStopErr == nil {
					removeCompletedHookLog(cmdStr, outputPath, outputReadErr)
				}
				log.InfoLog.Printf("post-worktree hooks cancelled for %s", run.worktreePath)
				return
			}
			if waitErr == nil {
				if scopeStopErr == nil {
					removeCompletedHookLog(cmdStr, outputPath, outputReadErr)
				}
				log.InfoLog.Printf("post-worktree hook %q completed successfully", cmdStr)
			} else {
				log.ErrorLog.Printf("post-worktree hook %q failed (full output: %s): %v\n%s", cmdStr, outputPath, waitErr, outputTail)
			}
			index++
		}
		journalTerminal = true
	}()
	return done
}

var stopHookScopeUnits = systemdunit.StopScopeUnits

var openHookLog = hooklog.Open

// Successful and deliberately cancelled hooks historically retained no output.
// Keep that contract (and avoid an unbounded success-log collection), while a
// failed run, an unconfirmed scope teardown, or one whose daemon disappeared
// before cleanup leaves the full file named in its diagnostic.
func removeCompletedHookLog(cmdStr, path string, readErr error) {
	if readErr != nil {
		return
	}
	if err := hooklog.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("post-worktree hook %q completed but its output log %s could not be removed: %v", cmdStr, path, err)
	}
}

// worktreePathScopeIdentity is the fallback scope identity for a hook run with
// no session id. It is a digest rather than the path itself because a unit name
// is a flat, sanitized namespace: two different paths must not collide into one
// name, and a sanitized path would.
func worktreePathScopeIdentity(worktreePath string) string {
	sum := sha256.Sum256([]byte(worktreePath))
	return "wt" + hex.EncodeToString(sum[:8])
}

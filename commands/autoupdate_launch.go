package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"

	"golang.org/x/term"
)

// reexecGuardEnv marks a process that an auto-update exec'd into. The
// throttle record already makes a second check a no-op, but that record is
// keyed on the version the new binary reports: a release whose stamped
// version disagrees with its tag would look "still out of date" and update
// again, forever. This env var is the belt to that suspenders — a process
// born of an update never updates again, whatever the versions say.
const reexecGuardEnv = "AGENT_FACTORY_AUTO_UPDATE_REEXECED"

// stdoutIsTTYFn reports whether stdout is a terminal. A var so tests can
// exercise both sides of the TTY gate.
var stdoutIsTTYFn = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// reexecFn replaces the current process image. A var so tests can observe the
// re-exec without actually vanishing mid-test. Only ever returns on failure.
var reexecFn = syscall.Exec

// autoUpdateNotice writes the launch-path progress line. A var so tests can
// capture it instead of scribbling on the test runner's stdout.
var autoUpdateNotice = func(format string, a ...any) { fmt.Printf(format, a...) }

// autoUpdateOnLaunch brings the user up to date before the TUI starts, then
// re-execs so they land in the new version on this launch rather than the
// next one. It runs synchronously and to completion: an update that installs
// after bubbletea owns the terminal could not be handed to the user without
// yanking the screen out from under them, and a half-finished download racing
// TUI init is worse than a short, explained pause.
//
// Every failure mode here is a silent skip. The update is a convenience; the
// launch is the thing the user asked for, and nothing in this path may block
// it, abort it, or print an error at it. Detail goes to the log.
func autoUpdateOnLaunch(cfg *config.Config) {
	// Consume the guard before anything else: it is a one-shot handoff from
	// the process that exec'd us, and it has to come out of the environment
	// either way. This TUI goes on to spawn tmux sessions and agents that
	// inherit our environment, and an `af` run inside one of those would read
	// a leaked guard as "never auto-update" — indefinitely, for as long as the
	// session lives.
	if _, reexeced := os.LookupEnv(reexecGuardEnv); reexeced {
		if err := os.Unsetenv(reexecGuardEnv); err != nil {
			log.WarningLog.Printf("auto-update: failed to clear %s: %v", reexecGuardEnv, err)
		}
		log.InfoLog.Printf("auto-update: already re-exec'd this launch; skipping")
		return
	}
	if cfg == nil {
		// The TUI launch hands us its already-resolved config; anyone else
		// gets the global one. A config that won't load must never block an
		// update, so fall through on error and run against the defaults.
		if loaded, err := config.LoadConfig(); err == nil {
			cfg = loaded
		}
	}
	if !autoUpdateEnabled(cfg) {
		log.InfoLog.Printf("auto-update: disabled")
		return
	}
	// A script, a pipe, or CI calling `af` must never get a binary swapped
	// out from under it: the caller pinned a version by installing one, and
	// self-mutating mid-pipeline is a surprise nobody asked for. An
	// interactive launch is the only place an update is welcome.
	if !stdoutIsTTYFn() {
		log.InfoLog.Printf("auto-update: stdout is not a terminal; skipping")
		return
	}

	channel := config.UpdateChannelStable
	if cfg != nil && cfg.UpdateChannel != "" {
		channel = cfg.UpdateChannel
	}

	// An install that failed only its bookkeeping still returns a version, so
	// log the error and re-exec anyway rather than stranding the user on the
	// old image.
	installed, err := autoUpdateForChannel(channel, autoUpdateCheckTimeout, autoUpdateDownloadBudget)
	if err != nil {
		log.ErrorLog.Printf("auto-update: %v", err)
	}
	if installed == "" {
		return
	}
	reexecIntoNewBinary(installed)
}

// reexecIntoNewBinary replaces this process with the freshly installed binary,
// preserving the original arguments so the user lands exactly where they meant
// to. On success it never returns. On failure the old — still perfectly
// working — process carries on into the TUI, and the user gets the one-line
// notice instead: the update is already on disk, so the next launch picks it
// up regardless.
//
// A successful exec skips every pending defer, root.go's log.Close() included.
// That is safe rather than lucky: the loggers write straight to the fd with no
// buffer to flush, and Go opens that fd O_CLOEXEC so the kernel closes it for
// us. Close() would only add the cosmetic "wrote logs to …" note, which the
// replacement process has no business printing.
func reexecIntoNewBinary(installedVersion string) {
	fallback := func(reason error) {
		log.WarningLog.Printf("auto-update: installed %s but could not re-exec: %v", installedVersion, reason)
		autoUpdateNotice("af updated to v%s — restart to use it.\n", installedVersion)
	}

	execPath, err := osExecutableFn()
	if err != nil {
		fallback(fmt.Errorf("failed to find executable: %w", err))
		return
	}
	// Exec the real binary rather than the symlink that pointed at it: the
	// new bytes were written through the resolved path.
	resolvedPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		fallback(fmt.Errorf("failed to resolve executable path: %w", err))
		return
	}

	// Nothing is announced here: the download already printed why the launch
	// paused, and a successful exec drops the user straight into the new
	// version's TUI, which would wipe the line anyway.
	env := append(os.Environ(), reexecGuardEnv+"=1")
	if err := reexecFn(resolvedPath, os.Args, env); err != nil {
		fallback(err)
	}
}

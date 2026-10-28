// Package afhome holds the abandoned-daemon write-side latch: nothing this
// process writes may re-create an AF home it has already observed present and
// then seen deleted (#3842/#3845/#3850).
//
// #3843 closed the socket binds, where filepath.Dir(socketPath) IS the home and
// the os.MkdirAll at each bind site was creating it. #3845 closed the state
// writes: every atomic write and every file lock goes through
// config.ensureStorageParent, whose MkdirAll creates the home as a parent of
// whatever it was asked for, and a handful of call sites MkdirAll a home
// subdirectory (events/, logs/, locks/, vscode/) before any of that is reached.
// #3850 closed the SESSION-LAUNCH path, where every unguarded MkdirAll runs
// before the first write a latch could refuse: the plugin dir on every claude
// launch, the agent-skill dirs, the tmux server log, the worktree root under
// worktree_root=subdirectory, and the per-agent account directories.
//
// Any one of them defeats watchDaemonHome (#1093/#1094): applyHomeCheck clears
// its consecutive-miss counter on a successful stat, so a daemon that resurrects
// its own home never observes the deletion and keeps firing schedules forever.
// #1093 was a leaked debug daemon spawning a session nightly for 23 days; the
// #3842 census found 9,892 /tmp/af-* directories on the maintainer's box.
//
// Why a LATCH rather than "the home must exist":
//
// A missing home is not the same fact in every process. `af config set` on a
// fresh machine legitimately creates the home on its first write, and so does
// the first `af` of any kind — refusing there would break the install path. What
// is never legitimate is a process that SAW the home and then re-creates it, so
// the precondition is scoped to exactly that: a home observed present, and now
// positively gone. A process that never armed the latch behaves as it always
// has, which is every CLI invocation.
//
// The daemon arms it once at startup, after acquireHomeLock has created the home
// (see daemon/daemon.go), and releases it on the way out. Resolving the home
// once there — rather than re-resolving GetConfigDir on every write — is also
// what keeps the check cheap on the hot path and immune to an AGENT_FACTORY_HOME
// that changes under a running process.
//
// # Why this is a leaf package rather than part of config
//
// The mechanism started in config (#3845), which is where its callers were. The
// launch path put it out of reach of two of them: config imports both log and
// session/tmux, so neither can import config back, and both create directories
// that are the AF home or sit inside it. Holding the latch in a package that
// depends on nothing but internal/pathutil is what lets every layer reach it.
// config re-exports it (config.MkdirAllUnderAFHome, config.MarkAFHomePresent,
// config.ErrAFHomeRemoved) so the packages that already depend on config keep
// their existing spelling.
package afhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// ErrRemoved reports that a write or directory creation was refused because the
// AF home this process observed at startup has since been deleted. The daemon is
// abandoned at that point and watchDaemonHome will shut it down; re-creating the
// home is what used to stop that from ever happening.
//
// Callers match on it with errors.Is to tell this deliberate refusal from an I/O
// failure. config.ErrAFHomeRemoved is this same value.
var ErrRemoved = errors.New("agent-factory home was removed after startup")

// observed is the latch. Empty path means unarmed, which is the state every CLI
// process stays in.
//
// gen makes release idempotent and correctly scoped: the daemon package runs
// RunDaemon in-process in its own tests, so two arms can overlap, and the first
// one's release must not clear the latch the second is relying on.
var observed struct {
	mu   sync.RWMutex
	path string
	gen  uint64
}

// MarkPresent latches home as observed-present for this process and returns the
// release. It is the daemon-startup arming; nothing else calls it.
//
// It only latches what it can SEE. A home it cannot stat is not "observed
// present", so arming against one returns an error and arms nothing — a daemon
// that somehow reached startup with no home then behaves exactly as it does
// today instead of refusing every write for the rest of its life. The returned
// release is always non-nil so callers can `defer` it before checking the error.
func MarkPresent(home string) (func(), error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return func() {}, fmt.Errorf("resolve AF home %s: %w", home, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return func() {}, fmt.Errorf("inspect AF home %s: %w", abs, err)
	}
	if !info.IsDir() {
		return func() {}, fmt.Errorf("AF home %s is not a directory", abs)
	}

	observed.mu.Lock()
	observed.gen++
	mine := observed.gen
	observed.path = abs
	observed.mu.Unlock()

	return func() {
		observed.mu.Lock()
		defer observed.mu.Unlock()
		if observed.gen == mine {
			observed.path = ""
		}
	}, nil
}

// MkdirAll is os.MkdirAll for a directory that can be at or inside the AF home.
//
// Use it wherever such a directory is created outside the atomic-write path —
// events/, logs/, locks/, vscode/, plugin/, worktrees/, accounts/, and the home
// itself where a caller creates it directly. os.MkdirAll creates every missing
// ancestor, so each of those calls is a way to re-create the whole home, and
// each one runs BEFORE the write that would otherwise have been caught by
// config.ensureStorageParent. On the session-launch path that ordering is the
// whole defect (#3850): the launch's MkdirAll runs before the create's persist,
// so the home is already back by the time a guarded write is reached.
//
// It is an exact os.MkdirAll when the latch is unarmed (every CLI process) or
// when the home is present, which is why converting a call site is a no-op for
// everything except an abandoned daemon.
//
// TestNoBareMkdirAllUnderTheAFHome enforces that new call sites reach here.
func MkdirAll(dir string, perm os.FileMode) error {
	if err := RequirePresent(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, perm)
}

// RequirePresent refuses when path is at or inside a home this process observed
// present and that is now positively gone.
//
// Only a POSITIVE observation of absence refuses, mirroring applyHomeCheck,
// updateDriver.homeExists and requireDaemonHomePresent: an inconclusive stat
// (EACCES, EIO) leaves the home's fate unknown, and refusing on "I could not
// tell" would stop a healthy daemon's writes on the strength of a transient
// error. The write proceeds and the kernel reports what it actually finds.
func RequirePresent(path string) error {
	observed.mu.RLock()
	home := observed.path
	observed.mu.RUnlock()
	if home == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		// Unresolvable is not evidence of anything; leave the outcome to the
		// syscall the caller is about to make.
		return nil
	}
	if !pathutil.IsAtOrInside(abs, home) {
		return nil
	}
	if _, err := os.Stat(home); err == nil || !os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("%w: %s was deleted out from under this daemon, so it is abandoned; "+
		"refusing to re-create the home for %s (the daemon shuts itself down on its next home check)",
		ErrRemoved, home, abs)
}

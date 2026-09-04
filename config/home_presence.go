package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// The write-side half of #3842/#3845: nothing this process writes may re-create
// an AF home it has already observed present and then seen deleted.
//
// #3843 closed the socket binds, where filepath.Dir(socketPath) IS the home and
// the os.MkdirAll at each bind site was creating it. The doors left open were
// the ordinary ones: every atomic write and every file lock goes through
// ensureStorageParent, whose MkdirAll creates the home as a parent of whatever
// it was asked for, and a handful of call sites MkdirAll a home subdirectory
// (events/, logs/, locks/, vscode/) before any of that is reached.
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

// ErrAFHomeRemoved reports that a write or directory creation was refused
// because the AF home this process observed at startup has since been deleted.
// The daemon is abandoned at that point and watchDaemonHome will shut it down;
// re-creating the home is what used to stop that from ever happening.
//
// Callers match on it with errors.Is to tell this deliberate refusal from an I/O
// failure.
var ErrAFHomeRemoved = errors.New("agent-factory home was removed after startup")

// observedAFHome is the latch. Empty path means unarmed, which is the state
// every CLI process stays in.
//
// gen makes release idempotent and correctly scoped: the daemon package runs
// RunDaemon in-process in its own tests, so two arms can overlap, and the first
// one's release must not clear the latch the second is relying on.
var observedAFHome struct {
	mu   sync.RWMutex
	path string
	gen  uint64
}

// MarkAFHomePresent latches home as observed-present for this process and
// returns the release. It is the daemon-startup arming; nothing else calls it.
//
// It only latches what it can SEE. A home it cannot stat is not "observed
// present", so arming against one returns an error and arms nothing — a daemon
// that somehow reached startup with no home then behaves exactly as it does
// today instead of refusing every write for the rest of its life. The returned
// release is always non-nil so callers can `defer` it before checking the error.
func MarkAFHomePresent(home string) (func(), error) {
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

	observedAFHome.mu.Lock()
	observedAFHome.gen++
	mine := observedAFHome.gen
	observedAFHome.path = abs
	observedAFHome.mu.Unlock()

	return func() {
		observedAFHome.mu.Lock()
		defer observedAFHome.mu.Unlock()
		if observedAFHome.gen == mine {
			observedAFHome.path = ""
		}
	}, nil
}

// MkdirAllUnderAFHome is os.MkdirAll for a directory at or inside the AF home.
//
// Use it wherever a home subdirectory is created outside the atomic-write path —
// events/, logs/, locks/, vscode/, and the home itself where a caller creates it
// directly. os.MkdirAll creates every missing ancestor, so each of those calls
// is a way to re-create the whole home, and each one runs BEFORE the write that
// would otherwise have been caught by ensureStorageParent.
//
// It is an exact os.MkdirAll when the latch is unarmed (every CLI process) or
// when the home is present, which is why converting a call site is a no-op for
// everything except an abandoned daemon.
func MkdirAllUnderAFHome(dir string, perm os.FileMode) error {
	if err := requireObservedAFHomePresent(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, perm)
}

// requireObservedAFHomePresent refuses when path is at or inside a home this
// process observed present and that is now positively gone.
//
// Only a POSITIVE observation of absence refuses, mirroring applyHomeCheck,
// updateDriver.homeExists and requireDaemonHomePresent: an inconclusive stat
// (EACCES, EIO) leaves the home's fate unknown, and refusing on "I could not
// tell" would stop a healthy daemon's writes on the strength of a transient
// error. The write proceeds and the kernel reports what it actually finds.
func requireObservedAFHomePresent(path string) error {
	observedAFHome.mu.RLock()
	home := observedAFHome.path
	observedAFHome.mu.RUnlock()
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
		ErrAFHomeRemoved, home, abs)
}

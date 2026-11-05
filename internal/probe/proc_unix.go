//go:build !windows

package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// probeExecSupported reports whether this platform can deliver the lifecycle
// guarantee the credential boundary requires: a probe run owns a process
// group, so a timeout — or any exit — can end every process it started,
// including a launcher that spawns a native binary. Unix delivers that with
// Setpgid plus group signals.
const probeExecSupported = true

// sweepProcGroupBound bounds the post-SIGKILL wait for the process group to
// empty. SIGKILL delivery is not instantaneous: a killed descendant is a
// zombie under init until reaped, and a process parked in an uninterruptible
// sleep reports alive until the kernel lets it die. The wait is what makes
// "the cap stays occupied until everything the run spawned is gone" honest
// instead of asserted.
const sweepProcGroupBound = 2 * time.Second

// setupProcGroup puts the child in its own process group so every descendant
// it spawns shares one signal target. Without it a launcher like the npm
// codex shim receives the kill while the native binary it forked keeps
// running under live credentials — still spending quota after af has counted
// the probe done.
func setupProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// groupPGID returns the process group id a group-led child owns: its own pid.
// The leader may already be dead and reaped — the group persists under that
// pgid — so the pgid is taken from construction, never re-queried.
func groupPGID(c *exec.Cmd) (int, bool) {
	if c == nil || c.Process == nil {
		return 0, false
	}
	return c.Process.Pid, true
}

// cancelViaGroup is exec.Cmd.Cancel for the probe: the deadline's first word
// with the child is SIGINT to the whole group, not SIGKILL to the launcher.
// Grace matters here, not courtesy: the CLI saves credentials
// truncate-then-write, so a mid-refresh SIGKILL can destroy the very
// credential the run was checking. os/exec waits WaitDelay after this
// returns nil, then kills the direct child — the group gets the grace
// window, and sweepProcGroup settles whatever survives it.
func cancelViaGroup(c *exec.Cmd) func() error {
	return func() error {
		pgid, ok := groupPGID(c)
		if !ok {
			return os.ErrProcessDone
		}
		if err := unix.Kill(-pgid, unix.SIGINT); err != nil {
			if err == unix.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}

// sweepProcGroup ends whatever the run left behind. It runs after every exec
// — not only on timeout — because a process can outlive a clean exit exactly
// the way it outlives a killed one. The leader's pid IS the pgid (setupProcGroup
// made it group leader), so the group is addressable even after the leader is
// reaped. The semaphore slot releases only after this returns.
func sweepProcGroup(c *exec.Cmd) {
	pgid, ok := groupPGID(c)
	if !ok {
		return // the executor seam never started a process
	}
	_ = unix.Kill(-pgid, unix.SIGKILL)
	deadline := time.Now().Add(sweepProcGroupBound)
	for {
		if err := unix.Kill(-pgid, 0); err == unix.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			// The group resisted SIGKILL within the bound — an
			// uninterruptible sleep is beyond af's power. The slot
			// releases; the residual is disclosed, not hidden.
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// accountHomeKey returns the flight/cache identity of a resolved account
// home: the filesystem object itself (device:inode), not the spelling the
// caller used. Two names that resolve to the same directory — a case
// variant on a case-insensitive filesystem, a hardlinked or re-mounted
// registry path — share one flight and one cache entry, because they are
// one credential home.
func accountHomeKey(dir string, info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "path:" + filepath.Clean(dir)
	}
	return "devino:" + strconv.FormatUint(uint64(st.Dev), 10) + ":" +
		strconv.FormatUint(uint64(st.Ino), 10)
}

// holdAuthLock takes an exclusive, non-blocking flock on an auth.lock file a
// newer Codex version may use to serialize credential refreshes. A held lock
// means a live copy is refreshing — the probe must not run; an acquired lock
// serializes the probe's whole run against any copy honoring the same lock.
// The file must exist (checked by the caller) and must be a real file,
// never a followed symlink.
func holdAuthLock(path string) (release func(), err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errSymlinkedAuthLock
	}
	f, err := os.Open(path) // O_RDONLY: flock needs no write
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

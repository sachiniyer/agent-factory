package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// lockedTarget is the ONE resolution a followed-lock body may touch: the file
// the lock is held on, plus the path the caller named.
//
// It exists because resolving more than once is the defect (#3688). The lock
// resolves the link to decide which .lock to take; if the body then reads the
// LINK and the writer resolves the LINK again, a link retargeted mid-operation
// — stow, chezmoi, a branch switch in the dotfiles repo — points that read and
// that write at a file the lock does not cover, while a peer af that resolved
// after the move holds a different .lock over the very same file. That is the
// "two different .lock files and no mutual exclusion" outcome the comment on
// withFollowedFileLock says the design rules out, reached by a moving link
// instead of by two static aliases.
//
// So the resolution is established once and then carried: file is what the body
// reads and writes, and link is only what the user is shown, what the AF-home
// hardening is judged against, and what the symlink notice names.
//
// The type is unexported ON PURPOSE, and so is withFollowedFileLock. Following a
// link is a promise decided for the global config alone (#3660, #3672), and
// TestFollowingWriterStaysInsideTheConfigPackage fences the exported writer by
// name; a handle whose write method is reachable from another package would be
// that fence with a hole in it. Here the follower cannot leave config/ at all.
type lockedTarget struct {
	// link is the path the caller asked for. It may be a symlink, and it is
	// what results, messages and source provenance should name — the user
	// arranged that path and does not think of their config by its target.
	link string
	// file is the file the lock covers. Read and write THIS one; nothing inside
	// the critical section may re-derive it from link.
	file string
}

// confirm reports that link still resolves to the file the lock was taken on,
// and names both ends when it does not.
//
// Pinning is what makes the write CORRECT — it lands on the locked file
// whatever the link does afterwards — and this is what makes a moved link LOUD
// rather than silently rewriting a file the user has redirected away from. It
// runs twice: on acquisition, because the resolve happens before a wait for the
// lock that can be arbitrarily long, and again at the write, because the body's
// read, parse and edit all happen in between.
//
// It is a guard, not the mechanism. Check-then-act is not atomic, so a link that
// moves after the check still leaves the write landing on the locked file; that
// outcome is the pinning's, and this only decides whether the user hears about
// it.
func (t lockedTarget) confirm() error {
	current, err := resolveWriteTarget(t.link)
	if err != nil {
		return fmt.Errorf("%s no longer resolves to %s, the file af locked: %w", t.link, t.file, err)
	}
	if current == t.file {
		return nil
	}
	// One wording for both call sites: this fires while queued for the lock and
	// again mid-operation, so it says the link moved rather than guessing when.
	return fmt.Errorf("%s moved while af was working on it: it now resolves to %s, "+
		"but the lock this operation holds covers %s — nothing was written, because a peer af "+
		"may be rewriting that other file under its own lock; re-run the command",
		t.link, current, t.file)
}

// write replaces the locked file atomically. The bytes land on the pinned file;
// link goes along only so the AF-home hardening and the one-line notice still
// see the path the user named.
func (t lockedTarget) write(data []byte, perm os.FileMode) error {
	if err := t.confirm(); err != nil {
		return err
	}
	return atomicWrite(t.link, t.file, data, perm)
}

// followedLockRaceHookForTest, when non-nil, runs after the target has been
// pinned and before the wait for the lock begins — the window in which a peer's
// stow or chezmoi run repoints the link while this process is queued behind
// another af. Tests use it to drive that window deterministically, as
// convertRaceHookForTest and materializeRaceHookForTest do for theirs.
var followedLockRaceHookForTest func()

// withFollowedFileLock is WithFileLock for a path whose write FOLLOWS a symlink
// — today only the global config.
//
// The lock has to resolve because the write does, and the two must agree or the
// lock stops meaning anything: once two aliases (two AF homes pointing at one
// dotfiles config) redirect their writes to a single file, locking each
// unresolved path produces two different .lock files and no mutual exclusion
// over the file both are rewriting (#3660 review).
//
// Agreeing once is not enough, which is why fn is handed a lockedTarget rather
// than the path it passed in: everything the body reads and writes has to be
// that same resolution, or a link that moves mid-operation reintroduces exactly
// the disagreement above (#3688).
//
// It is deliberately NOT what plain WithFileLock does. A caller that does not
// follow links writes a real file at its own path, so its lock belongs there —
// and resolving would drop a .lock file into whatever directory the link points
// at, which for a dotfiles repository is somebody's tracked working tree.
func withFollowedFileLock(path string, fn func(lockedTarget) error) error {
	// Surface a broken link HERE rather than locking the unresolved path and
	// letting the callback discover it. Callbacks read the file before they
	// reach the writer, so a silent fallback turned the both-ends error this
	// change promises into a bare ENOENT naming only config.toml
	// (#3660 review).
	target, err := resolveWriteTarget(path)
	if err != nil {
		return err
	}
	locked := lockedTarget{link: path, file: target}
	if followedLockRaceHookForTest != nil {
		followedLockRaceHookForTest()
	}
	return WithFileLock(target, func() error {
		// Waiting for the lock is unbounded, and the resolve above happened
		// before the wait. Re-check before the body does any work — reading,
		// parsing and editing a file the link no longer names is wasted at best
		// and, if the result were written, wrong (#3688).
		if err := locked.confirm(); err != nil {
			return err
		}
		return fn(locked)
	})
}

// TryWithFileLock is WithFileLock for callers that must not wait: it runs fn
// under the same exclusive flock, but only if the lock is free right now.
// It reports whether the lock was acquired; when it was not, fn never runs and
// the caller should treat the work as already in hand elsewhere rather than
// queue behind it. Use this on latency-sensitive paths (a user is waiting)
// where duplicating another process's work is pointless — blocking there turns
// a peer's slow operation into an unexplained hang of your own.
func TryWithFileLock(path string, fn func() error) (acquired bool, err error) {
	lockPath := path + ".lock"

	if err := ensureStorageParent(lockPath); err != nil {
		return false, fmt.Errorf("failed to create lock directory: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return false, fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	current, err := lockFileIsCurrent(f, lockPath)
	if err != nil {
		return false, fmt.Errorf("failed to validate file lock on %s: %w", lockPath, err)
	}
	if !current {
		return false, fmt.Errorf("file lock on %s was replaced while acquiring it", lockPath)
	}

	return true, fn()
}

// ErrLockTimeout is returned by WithFileLockTimeout when the flock could not be
// acquired within the caller's budget. Callers match on it (errors.Is) to tell a
// contended lock — retryable, the work never ran — from a real failure of fn.
var ErrLockTimeout = errors.New("timed out waiting for file lock")

// WithFileLockTimeout is WithFileLock bounded by a deadline: it runs fn under the
// same exclusive flock, but gives up with ErrLockTimeout rather than waiting
// forever. fn is never run unless the lock was actually held.
//
// It is the third point on the line TryWithFileLock and WithFileLock already
// stake out, and it exists for callers who must genuinely DO the work (so
// TryWithFileLock's "assume a peer has it in hand" contract is wrong for them)
// but who must also never hang (so WithFileLock is wrong too). The daemon's kill
// path is the motivating case: it must delete the record, and it holds a
// session-wide guard while doing it, so an unbounded wait here does not merely
// stall one write — it makes the session permanently undeletable (#1917).
//
// Acquisition polls LOCK_NB rather than parking in LOCK_EX because flock offers
// no timed acquire: a blocking Flock cannot be interrupted or given a deadline.
// Polling costs a wakeup every lockPollInterval while contended and trades away
// flock's (already unspecified) queueing fairness; the caller's budget bounds how
// long that lasts. A contended fd is reused across polling attempts, then its
// identity is compared with the lock path after acquisition. If the path was
// replaced while waiting, the stale inode is unlocked and the current path is
// opened before retrying.
func WithFileLockTimeout(path string, timeout time.Duration, fn func() error) error {
	lockPath := path + ".lock"

	if err := ensureStorageParent(lockPath); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
		}
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				current, validateErr := lockFileIsCurrent(f, lockPath)
				if validateErr != nil {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
					return fmt.Errorf("failed to validate file lock on %s: %w", lockPath, validateErr)
				}
				if current {
					defer f.Close()
					defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					return fn()
				}
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				if !time.Now().Before(deadline) {
					return fmt.Errorf("%w on %s after %s (lock file was replaced while waiting)", ErrLockTimeout, lockPath, timeout)
				}
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) {
				_ = f.Close()
				return fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
			}
			if !time.Now().Before(deadline) {
				_ = f.Close()
				return fmt.Errorf("%w on %s after %s (another agent-factory process is holding it)", ErrLockTimeout, lockPath, timeout)
			}
			// Sleep no longer than the remaining budget, so the effective wait
			// matches the caller's timeout instead of overshooting by up to a poll.
			wait := lockPollInterval
			if remaining := time.Until(deadline); remaining < wait {
				wait = remaining
			}
			time.Sleep(wait)
		}
	}
}

// lockPollInterval is how often WithFileLockTimeout re-attempts a contended
// flock. A var so tests can shorten it; production never reassigns.
var lockPollInterval = 20 * time.Millisecond

// WithFileLock acquires an exclusive flock on a .lock file adjacent to the target path,
// executes fn, and releases the lock. This ensures atomic read-modify-write sequences
// across multiple processes. It BLOCKS until the lock is free; see
// TryWithFileLock when a user is waiting on the result, or WithFileLockTimeout
// when the caller must do the work but must not hang (#1917).
func WithFileLock(path string, fn func() error) error {
	lockPath := path + ".lock"

	// Ensure the directory exists so the lock file can be created.
	if err := ensureStorageParent(lockPath); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			_ = f.Close()
			return fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
		}
		current, validateErr := lockFileIsCurrent(f, lockPath)
		if validateErr != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return fmt.Errorf("failed to validate file lock on %s: %w", lockPath, validateErr)
		}
		if current {
			defer f.Close()
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return fn()
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

func lockFileIsCurrent(f *os.File, lockPath string) (bool, error) {
	openedInfo, err := f.Stat()
	if err != nil {
		return false, err
	}
	pathInfo, err := os.Stat(lockPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(openedInfo, pathInfo), nil
}

// AtomicWriteFile writes data to a temporary file in the same directory as path
// and atomically renames it to path. This prevents partial writes from being
// visible to readers.
//
// A symlink at path is REPLACED by the regular file, which is what os.Rename
// does on its own. That is the historical behaviour and it is deliberate for
// callers who neither follow (AtomicWriteFileFollowingLink) nor refuse
// (AtomicWriteFileRefusingLink) — see #3672 for the per-caller table.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	// Its own path IS its target: this writer replaces the file at the path it
	// was given, link or not.
	return atomicWrite(path, path, data, perm)
}

// AtomicWriteFileRefusingLink is AtomicWriteFile for the files af MANAGES: the
// bearer token, the daemon PID file, the autostart unit/plist, the
// editor-origin secret, the VS Code owner record, the upgrade interlock's
// executable swap, the auto-update check cache, the event-queue cursor, and the
// plugin/skill files af regenerates (#3672).
//
// When path is a symlink it writes nothing and returns an error naming BOTH
// ends, wrapping ErrManagedFileSymlink. These are af's own files at paths af
// chose, so a link there is not an arrangement af can honour either way:
// replacing it destroys whatever the user set up, and writing through it is a
// promise ("af will maintain a file wherever you point this") that none of these
// callers ever offered. Failing closed hands the decision back to the person who
// made the link.
//
// Refusing is also what keeps the write/cleanup asymmetry #3672 is titled after
// from arising at all. A write that FOLLOWED a link, cleaned up by a plain
// os.Remove of the same path, unlinks the link while its target keeps af's
// content — a systemd unit af no longer knows about, still being read. Callers
// that delete what they wrote pair this with RemoveFileRefusingLink, so neither
// end ever acts on a link.
//
// This is a POLICY guard, not a security boundary. It lstats and then renames,
// so a link swapped in between the two is still replaced — os.Rename never
// follows a final-component link, so the outcome there is today's behaviour, not
// a write through a link. Defending a path against a racing attacker needs the
// in-repo writer's pinned-directory shape (config/inrepo.go), and #3697 tracks
// the parent-directory case separately.
func AtomicWriteFileRefusingLink(path string, data []byte, perm os.FileMode) error {
	// The refusal is FIRST, before atomicWrite's ensureStorageParent, so a
	// refused write leaves the filesystem exactly as it found it: no created
	// parent, no AF-home chmod on a path af just declined to touch.
	if err := RefuseManagedFileSymlink(path); err != nil {
		return err
	}
	// Its own path is its target, as for the plain writer: nothing here follows
	// anything, it just stops when a link is in the way.
	return atomicWrite(path, path, data, perm)
}

// ErrManagedFileSymlink reports that an af-managed path is a symlink, so the
// write or delete was refused. Callers match on it (errors.Is) to tell this
// deliberate refusal from an I/O failure; the message names both ends.
var ErrManagedFileSymlink = errors.New("af-managed file is a symlink")

// RemoveFileRefusingLink is os.Remove for a path an af-managed writer owns.
//
// It refuses a symlink for the same reason AtomicWriteFileRefusingLink does, and
// it is the half #3672 was filed about: daemon/autostart.go wrote the unit with
// one helper and cleaned up with a bare os.Remove, so a failed install or an
// uninstall would have unlinked the LINK while its target held af's content. A
// path af will not write through is a path af must not unlink either.
//
// A missing path returns the usual ENOENT, so callers keep their os.IsNotExist
// checks unchanged.
func RemoveFileRefusingLink(path string) error {
	if err := RefuseManagedFileSymlink(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// RefuseManagedFileSymlink returns the both-ends error when path is a symlink,
// and nil for anything else — including an absent path, which is an ordinary
// create or an already-done delete.
//
// A DANGLING link is refused too, and by the same rule rather than a special
// case: the policy is about the link's presence, not about what it resolves to.
//
// It is the check AtomicWriteFileRefusingLink and RemoveFileRefusingLink make,
// exported for callers that must take it before an action NEITHER of those
// performs, and where discovering the link later would already have done damage
// (#3672 review). Two such callers exist: InstallAutostart runs `launchctl
// bootout` before writing the plist, so a link found at the write would leave
// the running agent unloaded and nothing bootstrapped; and the event queue drops
// its cursor through an injectable removal seam, which a check inside
// RemoveFileRefusingLink cannot reach.
func RefuseManagedFileSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, readErr := readLinkTarget(path)
	if readErr != nil {
		return fmt.Errorf("%w: %s is a symlink whose target could not be read (%v); "+
			"af manages this file — remove the link and let af write a real file there",
			ErrManagedFileSymlink, path, readErr)
	}
	return fmt.Errorf("%w: %s is a symlink to %s; af manages this file and will neither "+
		"write through the link nor replace it — remove the link and let af write a real "+
		"file there, or point af at the other location instead",
		ErrManagedFileSymlink, path, target)
}

// readLinkTarget returns what a symlink points at, as an absolute path. A
// relative target is joined against the LINK's directory, which is how the
// kernel resolves it; reporting the raw relative text would name a path that
// does not exist from the reader's working directory.
func readLinkTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return target, nil
}

// AtomicWriteFileFollowingLink is AtomicWriteFile for the files af rewrites on
// the user's behalf rather than owns: the global config (#3660) and the task
// store (#3672).
//
// When path is a symlink it writes the file the link names, leaving the link in
// place. That is right for config.toml — the user pointed it at their dotfiles
// deliberately and every editor writes through it — and it is right for
// tasks.json for the same reason: it is user-authored content a user may
// reasonably keep beside their config. It is NOT right by default, which is why
// it is a separate function rather than a flag on the shared one. af's own
// managed files refuse a link outright (AtomicWriteFileRefusingLink), and the
// in-repo config follows one only as far as the repository goes, with an
// O_NOFOLLOW pinned directory, because a checked-in config belongs to a
// repository a clone does not control.
//
// Callers outside config/ and task/ are refused by
// TestFollowingWriterStaysInsideTheConfigPackage rather than by the compiler,
// so the promise cannot spread silently.
//
// Nothing in THIS package calls it. Since #3688 every followed write inside
// config/ happens under withFollowedFileLock and goes through the resolution
// that lock pinned, because resolving a second time inside a critical section
// is the bug that issue is about. Its one caller is task/'s tasks.json writer
// (#3672), which is exactly what a followed write looks like with no lock to
// pin one: withFollowedFileLock and lockedTarget are unexported on purpose, so
// task/ resolves here instead. That leaves tasks.json with the #3688 shape a
// link retargeted MID-OPERATION can still catch — the read resolves at open
// time and this write resolves again — which is the class #3697 owns and #3672
// deliberately left alone. It is not the #3660 bug: an unmoving link is
// followed correctly, which is the promise tasks.json was put on this side
// for.
func AtomicWriteFileFollowingLink(path string, data []byte, perm os.FileMode) error {
	target, err := resolveWriteTarget(path)
	if err != nil {
		return err
	}
	return atomicWrite(path, target, data, perm)
}

// atomicWrite stages data beside target and renames it into place.
//
// It is always TOLD where the bytes go; it never decides. path is the caller's
// own path — what the AF-home hardening is judged against and what a symlink
// notice names — and target is the file being replaced. The two differ only
// when a link is being followed, and whoever followed it did the resolving
// (#3688). A writer that REFUSES a link decides before calling here, for the
// same reason: the refusal has to happen before ensureStorageParent, so a
// refused write leaves the filesystem exactly as it found it (#3672).
func atomicWrite(path, target string, data []byte, perm os.FileMode) error {
	// ensureStorageParent runs on the ORIGINAL path rather than on target, and
	// that is load-bearing: secureAFHomeForPath hardens the AF home only when
	// the write path is at or inside it, so passing the resolved target would
	// move the path outside the home and silently skip that hardening — for
	// exactly the users who link their config into a dotfiles repo (#3660).
	if err := ensureStorageParent(path); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", filepath.Dir(path), err)
	}

	if target != path {
		noticeSymlinkWrite(path, target)
		// Do not widen the real file. Callers pass 0644 because that is the
		// mode for a config af itself created; a target the user keeps at
		// 0600 in their dotfiles is a deliberate choice about a file af is
		// only rewriting, and following must not quietly relax it.
		if info, statErr := os.Stat(target); statErr == nil {
			perm = info.Mode().Perm()
		}
	}
	// The temp file lives in the TARGET's directory: os.Rename cannot cross
	// filesystems, and a link into another mount is the whole point of the
	// arrangement when one is being followed.
	dir := filepath.Dir(target)

	tmp, err := os.CreateTemp(dir, filepath.Base(target)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Clean up the temp file on any error path.
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", target, err)
	}

	// Rename succeeded: data is visible on disk. The contract
	// "err == nil ⟺ data is persisted" must hold from this point on, so the
	// parent-directory fsync below (which only affects crash durability, not
	// visibility) becomes best-effort. Mark success so the deferred temp-file
	// cleanup is a no-op and downstream callers don't roll back persisted data.
	success = true

	// Fsync the parent directory to ensure the rename (new directory entry) is
	// persisted across a crash. Failures here are logged but not returned --
	// the data is already visible to readers.
	dirFd, err := os.Open(dir)
	if err != nil {
		log.WarningLog.Printf("atomicWrite: failed to open directory %s for post-rename sync: %v", dir, err)
		return nil
	}
	if err := dirFd.Sync(); err != nil {
		log.WarningLog.Printf("atomicWrite: failed to fsync directory %s after rename of %s: %v", dir, target, err)
	}
	if err := dirFd.Close(); err != nil {
		log.WarningLog.Printf("atomicWrite: failed to close directory %s after post-rename sync of %s: %v", dir, target, err)
	}
	return nil
}

// ensureStorageParent creates path's parent without changing the historical
// 0755 policy for generic callers (upgrade binaries, autostart files, repo
// plugin files). When path is inside the AF home, it first secures that root.
// Creating a descendant with MkdirAll(0755) can then never accidentally create
// the default secret-bearing home world-readable (#2197).
func ensureStorageParent(path string) error {
	if err := secureAFHomeForPath(path); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

// secureAFHomeForPath handles the single-owner boundary only for paths inside
// the configured AF home. A newly created home is always 0700, and the default
// ~/.agent-factory is tightened on upgrade. An existing custom home is left
// alone: AGENT_FACTORY_HOME explicitly supports broad caller-owned directories
// such as "~", and a file helper must never chmod those. A default-name symlink
// is custom ownership too: AF neither chmods its target nor blocks startup over
// that mode. AtomicWriteFile and the lock helpers are generic, so paths elsewhere
// are left alone too.
func secureAFHomeForPath(path string) error {
	afHome, err := GetConfigDir()
	if err != nil {
		// A generic write outside config storage must not start depending on a
		// resolvable AGENT_FACTORY_HOME. Callers writing inside it obtained their
		// path from GetConfigDir already and will have surfaced that error there.
		return nil
	}
	absHome, err := filepath.Abs(afHome)
	if err != nil {
		return fmt.Errorf("resolve AF home: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve storage path: %w", err)
	}
	if !pathutil.IsAtOrInside(absPath, absHome) {
		return nil
	}
	info, statErr := os.Lstat(absHome)
	created := false
	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(absHome, 0o700); err != nil {
			return fmt.Errorf("create AF home: %w", err)
		}
		// Reinspect after creation. Besides making chmod independent of umask,
		// this catches another process winning the missing-path race with a
		// symlink instead of blindly following it below.
		info, statErr = os.Lstat(absHome)
		created = true
	}
	if statErr != nil {
		return fmt.Errorf("inspect AF home: %w", statErr)
	}
	// Environment presence does not make a path custom: users commonly export an
	// alias of $HOME/.agent-factory to pin the default explicitly. Direction does
	// matter, though. An alias INTO a concrete default is AF-owned and repairable;
	// when the default name itself is a symlink, its target remains caller-owned.
	// concreteDefaultAFHome returns the path AF may safely chmod, never the alias.
	defaultRepairPath := concreteDefaultAFHome(absHome)
	if defaultRepairPath == "" && !created {
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Chmod follows symlinks. Never let a default ~/.agent-factory symlink
		// trick this repair into changing an unrelated target directory.
		target, err := os.Stat(absHome)
		if err != nil {
			return fmt.Errorf("inspect AF home symlink target: %w", err)
		}
		if !target.IsDir() {
			return fmt.Errorf("AF home %s is a symlink whose target is not an owner-only directory", absHome)
		}
		if target.Mode().Perm() != 0o700 {
			if defaultRepairPath == "" {
				return fmt.Errorf("AF home %s is a symlink whose target is not an owner-only directory", absHome)
			}
			// Repair the concrete default, not the user-provided alias. That keeps a
			// retargeted alias from redirecting chmod to a caller-owned directory.
			if err := os.Chmod(defaultRepairPath, 0o700); err != nil {
				return fmt.Errorf("secure AF home: %w", err)
			}
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("AF home %s is not a directory", absHome)
	}
	repairPath := absHome
	if defaultRepairPath != "" {
		repairPath = defaultRepairPath
	}
	if err := os.Chmod(repairPath, 0o700); err != nil {
		return fmt.Errorf("secure AF home: %w", err)
	}
	return nil
}

// concreteDefaultAFHome returns the concrete default directory when absHome is
// another spelling of it. The direction is intentional: if the default name is
// itself a symlink, its target is caller-owned and no spelling of that target is
// permission-repairable here. This resolves both sides of the policy without
// turning symmetric path equality into symmetric ownership.
func concreteDefaultAFHome(absHome string) string {
	defaultHome, err := ConfigDirFor("")
	if err != nil {
		return ""
	}
	absDefault, err := filepath.Abs(defaultHome)
	if err != nil {
		return ""
	}
	defaultInfo, err := os.Lstat(absDefault)
	if err != nil || !defaultInfo.IsDir() || defaultInfo.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if pathutil.ResolveForCompare(absHome) != pathutil.ResolveForCompare(absDefault) {
		return ""
	}
	return absDefault
}

// resolveWriteTarget returns the file an atomic write should actually replace.
//
// os.Rename replaces a LINK rather than what it points at, so without this every
// config write turned a symlinked config.toml into a regular file: the target
// kept its old content, the link was gone, and nothing said so (#3660). A path
// that is not a symlink — including one that does not exist yet, which is an
// ordinary create — is returned unchanged.
//
// A DANGLING link is an error rather than a create. Following it would either
// fail obscurely inside CreateTemp or quietly materialize a file at a path the
// user believes already exists somewhere else; both hide the broken link, which
// is the thing they need to know.
func resolveWriteTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return "", fmt.Errorf("failed to inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	resolved, evalErr := filepath.EvalSymlinks(path)
	if evalErr == nil {
		return resolved, nil
	}
	// Name BOTH ends. "no such file or directory" against the link's own path
	// would send the reader looking at a link that is plainly there.
	target, readErr := readLinkTarget(path)
	if readErr != nil {
		return "", fmt.Errorf("failed to resolve the symlink %s: %w", path, evalErr)
	}
	return "", fmt.Errorf("%s is a symlink to %s, which cannot be resolved: %w; "+
		"af will not create it — repair the link or remove it and let af write a real file there",
		path, target, evalErr)
}

// symlinkWriteNotices keys the one-line notice by link path. Once per process,
// not once per write: af rewrites its config many times in a session and the
// fact does not change between them, so per-write would be the noise this
// repository keeps having to clean up.
var symlinkWriteNotices sync.Map

func noticeSymlinkWrite(link, target string) {
	if _, seen := symlinkWriteNotices.LoadOrStore(link, struct{}{}); seen {
		return
	}
	log.InfoLog.Printf("%s is a symlink · writing through it to %s", link, target)
}

func resetSymlinkWriteNotices() { symlinkWriteNotices.Clear() }

// refuseDanglingConfigLink reports the both-ends error when path is a symlink
// that does not resolve, and nil for anything else — including a path that is
// simply absent, which is an ordinary first run.
//
// The read paths need this separately from the write paths: a dangling link
// makes os.ReadFile return ENOENT, which is indistinguishable from "no config
// yet" unless somebody looks with Lstat (#3660 review).
func refuseDanglingConfigLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if _, err := resolveWriteTarget(path); err != nil {
		return err
	}
	return nil
}

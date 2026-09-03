package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	// name is filepath.Base(file): how that file is spelled INSIDE dirFD, which
	// is the only spelling any syscall in the critical section uses.
	name string
	// dir is filepath.Dir(file): the directory the lock file and the write both
	// live in, spelled the way the caller reached it. Messages and the identity
	// re-check use it; no write goes through it.
	dir string
	// dirResolved is what dir pointed at when the lock was taken, for naming
	// that end in a refusal. The decision is never made on this string —
	// see confirmDir.
	dirResolved string
	// dirFD is that same directory, held open for the length of the critical
	// section. Every syscall that creates, renames or stats inside it goes
	// through this fd rather than through dir, so a link ABOVE the file cannot
	// move the bytes after the lock was taken (#3697). It is owned by
	// withFollowedFileLock, which closes it once the body has returned; copies
	// of a lockedTarget never outlive that.
	dirFD int
}

// confirm reports that link still resolves to the file the lock was taken on
// AND that its directory is still the one the lock was taken in, naming both
// ends of whichever moved.
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
	if current != t.file {
		// One wording for both call sites: this fires while queued for the lock
		// and again mid-operation, so it says the link moved rather than
		// guessing when.
		return fmt.Errorf("%s moved while af was working on it: it now resolves to %s, "+
			"but the lock this operation holds covers %s — nothing was written, because a peer af "+
			"may be rewriting that other file under its own lock; re-run the command",
			t.link, current, t.file)
	}
	return t.confirmDir()
}

// confirmDir is the same guard one level up: the directory the lock was taken
// in is still the directory this path reaches.
//
// The file-level check above cannot see this case, and that is the whole of
// #3697. Repoint a symlinked AGENT_FACTORY_HOME under a perfectly ORDINARY
// regular config.toml and resolveWriteTarget is asked about a path whose last
// component is not a link, so it returns that path unchanged — the identical
// string before and after the move, so the comparison above holds while every
// path-based syscall in this operation has quietly started landing in another
// directory, beside a .lock file this process does not hold.
//
// Identity is dev/inode rather than a resolved path string on purpose: `stow
// -D && stow`, or a dotfiles checkout, removes the directory and makes a new
// one under the same name, which no comparison of strings can tell apart from
// nothing having happened at all.
func (t lockedTarget) confirmDir() error {
	var pinned unix.Stat_t
	if err := unix.Fstat(t.dirFD, &pinned); err != nil {
		return fmt.Errorf("failed to inspect %s, the directory af locked for %s: %w", t.dirResolved, t.link, err)
	}
	var now unix.Stat_t
	if err := unix.Stat(t.dir, &now); err != nil {
		return fmt.Errorf("%s no longer reaches %s, the directory af locked: %w", t.dir, t.dirResolved, err)
	}
	if sameInode(&pinned, &now) {
		return nil
	}
	moved := t.dir
	if resolved, err := filepath.EvalSymlinks(t.dir); err == nil {
		moved = resolved
	}
	return fmt.Errorf("the directory holding %s moved while af was working on it: %s now reaches %s, "+
		"but the lock this operation holds covers %s — nothing was written, because a peer af "+
		"may be rewriting the file in that other directory under its own lock; re-run the command",
		t.link, t.dir, moved, t.dirResolved)
}

// followedWriteRaceHookForTest, when non-nil, runs after confirm has passed and
// before the bytes move — the check-then-act window confirm cannot close. Tests
// use it to drive a retarget INTO that window, where only the pinned directory
// decides where the write lands.
var followedWriteRaceHookForTest func()

// write replaces the locked file atomically, inside the directory the lock was
// taken in. The bytes land on the pinned file; link goes along only so the
// AF-home hardening and the one-line notice still see the path the user named.
//
// It goes through the pinned fd rather than through atomicWrite because
// confirm is a check and this is the act. A path-based staging and rename
// re-resolve the directory at every syscall, so a home link repointed in the
// microseconds after confirm returns would put the temp file and the rename in
// a directory holding somebody else's .lock — the lost update of #3697, just
// through a narrower window than the one the check catches. Relative to an open
// fd there is no window: the directory cannot be renamed out from under an
// inode that is already open.
func (t lockedTarget) write(data []byte, perm os.FileMode) error {
	if err := t.confirm(); err != nil {
		return err
	}
	// The hardening is judged against the ORIGINAL path for the reason
	// atomicWrite gives: secureAFHomeForPath only acts on a path at or inside
	// the AF home, so handing it the resolved file would skip it for exactly
	// the users who link their config into a dotfiles repo (#3660).
	if err := ensureStorageParent(t.link); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", filepath.Dir(t.link), err)
	}
	if t.file != t.link {
		noticeSymlinkWrite(t.link, t.file)
		// Do not widen the real file. Callers pass 0644 because that is the
		// mode for a config af itself created; a target the user keeps at 0600
		// in their dotfiles is a deliberate choice about a file af is only
		// rewriting, and following must not quietly relax it.
		if existing, err := t.perm(); err == nil {
			perm = existing
		}
	}
	if followedWriteRaceHookForTest != nil {
		followedWriteRaceHookForTest()
	}
	return atomicWriteInOpenDir(t.dirFD, t.dir, t.name, data, perm)
}

// read returns the locked file's bytes, opened through the pinned directory.
//
// os.ReadFile(t.file) is NOT equivalent, and the difference is the read half of
// #3697: under a symlinked AGENT_FACTORY_HOME with a regular config.toml,
// t.file is still the unresolved alias, so the kernel resolves it afresh at
// open time. A link moved to another directory and moved BACK before the write
// would feed a read-modify-write somebody else's bytes and land the result on
// the locked file, with every check on either side of it passing (#3697
// review). Reading through the fd asks the directory this lock covers.
//
// Errors are wrapped as *os.PathError naming t.file so callers keep testing
// them with os.IsNotExist, and so a message reads the way os.ReadFile's did.
func (t lockedTarget) read() ([]byte, error) {
	fd, err := unix.Openat(t.dirFD, t.name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: t.file, Err: err}
	}
	f := os.NewFile(uintptr(fd), t.file)
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: t.file, Err: err}
	}
	return data, nil
}

// perm reports the locked file's current mode bits, asked of the pinned
// directory. Callers use it to carry an existing file's mode onto a rewrite
// rather than widening it to a fresh default.
func (t lockedTarget) perm() (os.FileMode, error) {
	var info unix.Stat_t
	if err := unix.Fstatat(t.dirFD, t.name, &info, 0); err != nil {
		return 0, &os.PathError{Op: "stat", Path: t.file, Err: err}
	}
	return os.FileMode(info.Mode & 0o777), nil
}

// availableBackupPath is availableBackupPath asked of the pinned directory: the
// same never-overwrite-an-existing-backup rule, decided about the directory the
// lock covers. Asking the path instead lets a retarget answer "free" about one
// directory and then write into another, where that name is a backup somebody
// still needs (#3697 review).
func (t lockedTarget) availableBackupPath() (string, error) {
	return availableBackupPathWith(t.file+".bak", t.siblingOccupied)
}

func (t lockedTarget) siblingOccupied(candidate string) bool {
	var info unix.Stat_t
	// AT_SYMLINK_NOFOLLOW for pathOccupied's reason: a dangling <config>.bak
	// link is a filesystem entry the user made, and a name it holds is taken.
	err := unix.Fstatat(t.dirFD, filepath.Base(candidate), &info, unix.AT_SYMLINK_NOFOLLOW)
	return err == nil || !errors.Is(err, unix.ENOENT)
}

// writeSibling replaces a file beside the locked one — today only the migration
// backup — inside the pinned directory. It takes the display path so callers
// keep reporting the name the user will look for.
func (t lockedTarget) writeSibling(path string, data []byte, perm os.FileMode) error {
	return atomicWriteInOpenDir(t.dirFD, t.dir, filepath.Base(path), data, perm)
}

// removeSibling deletes one, in the same directory writeSibling put it. A
// caller that wrote a backup and then could not finish must take back THAT
// backup, not whatever now sits at the same path in some other directory.
func (t lockedTarget) removeSibling(path string) error {
	if err := unix.Unlinkat(t.dirFD, filepath.Base(path), 0); err != nil {
		return &os.PathError{Op: "remove", Path: path, Err: err}
	}
	return nil
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
// And resolving the FILE once is not enough either, because a path can move
// without its last component being a link at all. A symlinked
// AGENT_FACTORY_HOME repointed under an ordinary regular config.toml leaves
// every string here identical while the lock and the write land in different
// directories, so the handle also carries the directory itself, held open
// (#3697). From here down, nothing in this operation reaches disk by a path
// that an external actor can still redirect.
//
// It is deliberately NOT what plain WithFileLock does. A caller that does not
// follow links writes a real file at its own path, so its lock belongs there —
// and resolving would drop a .lock file into whatever directory the link points
// at, which for a dotfiles repository is somebody's tracked working tree.
func withFollowedFileLock(path string, fn func(lockedTarget) error) error {
	locked, release, err := pinFollowedTarget(path)
	if err != nil {
		return err
	}
	defer release()
	if followedLockRaceHookForTest != nil {
		followedLockRaceHookForTest()
	}
	return locked.withLock(func() error {
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

// pinFollowedTarget establishes, once, everything the critical section is
// allowed to know about where the bytes go: the file the lock will cover, and
// the directory holding it, held open.
//
// The directory is opened WITHOUT O_NOFOLLOW, unlike the in-repo writer's. A
// symlinked AGENT_FACTORY_HOME is a supported arrangement — secureAFHomeForPath
// has a whole branch for it — so refusing the open would break the ordinary
// dotfiles user this pin is meant to protect. Following once and then holding
// the fd is what makes the pin correct: the resolution the lock is taken
// against is the resolution the write uses, whatever happens to the path after.
//
// The returned release closes that fd. Nothing derived from the handle may
// outlive it.
func pinFollowedTarget(path string) (lockedTarget, func(), error) {
	// Surface a broken link HERE rather than locking the unresolved path and
	// letting the callback discover it. Callbacks read the file before they
	// reach the writer, so a silent fallback turned the both-ends error this
	// change promises into a bare ENOENT naming only config.toml
	// (#3660 review).
	target, err := resolveWriteTarget(path)
	if err != nil {
		return lockedTarget{}, nil, err
	}
	// The directory has to exist before it can be held open, and the AF home
	// has to be hardened before anything is created inside it. Both are what
	// ensureStorageParent does, and it is given the ORIGINAL path for the reason
	// atomicWrite documents: a resolved target is outside the home for exactly
	// the dotfiles users the hardening is for (#3660).
	if err := ensureStorageParent(path); err != nil {
		return lockedTarget{}, nil, fmt.Errorf("failed to create lock directory: %w", err)
	}
	dir := filepath.Dir(target)
	dirFD, err := openPinnedDir(dir)
	if err != nil {
		return lockedTarget{}, nil, err
	}
	// Only for naming that end in a refusal; the identity comparison uses the
	// fd, so a directory that cannot be spelled resolvably is not a failure.
	dirResolved := dir
	if resolved, evalErr := filepath.EvalSymlinks(dir); evalErr == nil {
		dirResolved = resolved
	}
	locked := lockedTarget{link: path, file: target, name: filepath.Base(target), dir: dir, dirResolved: dirResolved, dirFD: dirFD}
	return locked, func() { _ = unix.Close(dirFD) }, nil
}

// openPinnedDir holds a directory open for the critical section.
//
// The ordinary open needs READ permission on the directory, which a path-based
// writer never did: it created its temp file, its lock and its rename with
// write+execute alone. So a config directory deliberately kept unreadable —
// mode 0300 — would have gone from working to failing every global config write
// (#3697 review). Where the platform can open a directory execute-only that
// costs nothing but the directory fsync, which such a directory could not have
// had anyway; where it cannot, this refuses and says which permission to add,
// because dropping back to an unpinned write would quietly reinstate the lost
// update this whole change exists to remove.
func openPinnedDir(dir string) (int, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err == nil {
		return fd, nil
	}
	if errors.Is(err, unix.EACCES) {
		if dirSearchOnlyFlag != 0 {
			if searchFD, searchErr := unix.Open(dir, dirSearchOnlyFlag|unix.O_DIRECTORY|unix.O_CLOEXEC, 0); searchErr == nil {
				return searchFD, nil
			}
		}
		return -1, fmt.Errorf("failed to open the directory %s: %w — af holds it open for the whole "+
			"config write so a moved symlink cannot redirect the bytes, which needs read permission on "+
			"it (chmod u+r %s)", dir, err, dir)
	}
	return -1, fmt.Errorf("failed to open the directory %s: %w", dir, err)
}

// withLock is WithFileLock over the pinned directory: the same adjacent .lock
// file, at the same path it has always been at, opened relative to the fd
// instead of by that path.
//
// The lock file does not move. openat(dirfd, "config.toml.lock") against a fd
// opened on the directory reaches the inode the kernel was already reaching
// when it resolved "~/.agent-factory/config.toml.lock" through the home link —
// one file, two spellings — so no user, linked home or not, finds a .lock
// anywhere new (#3697). What changes is that the lock and the write can no
// longer be talked into disagreeing about which directory that is.
//
// The replaced-inode retry is WithFileLock's, for its reason: a peer that
// removes the lock file between our open and our flock leaves us holding an
// exclusive lock on an unlinked inode, which excludes nobody.
func (t lockedTarget) withLock(fn func() error) error {
	name := t.name + ".lock"
	// Named by path in messages, because that is what the user can go look at.
	lockPath := t.file + ".lock"

	for {
		fd, err := unix.Openat(t.dirFD, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0644)
		if err != nil {
			return fmt.Errorf("failed to open lock file %s: %w", lockPath, err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("failed to acquire file lock on %s: %w", lockPath, err)
		}
		current, validateErr := lockFileIsCurrentAt(fd, t.dirFD, name)
		if validateErr != nil {
			_ = syscall.Flock(fd, syscall.LOCK_UN)
			_ = unix.Close(fd)
			return fmt.Errorf("failed to validate file lock on %s: %w", lockPath, validateErr)
		}
		if current {
			defer unix.Close(fd)
			defer syscall.Flock(fd, syscall.LOCK_UN)
			return fn()
		}
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = unix.Close(fd)
	}
}

// lockFileIsCurrentAt is lockFileIsCurrent through the pinned directory: the fd
// we flocked is still the file that name reaches. A name that reaches nothing
// is not current, which is the removed-and-not-yet-recreated case the caller
// retries.
func lockFileIsCurrentAt(fd, dirFD int, name string) (bool, error) {
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return false, err
	}
	var atName unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &atName, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	return sameInode(&opened, &atName), nil
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

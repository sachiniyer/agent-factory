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
	// retargeted is set when the path now resolves somewhere other than
	// dirResolved while the lock was being opened; the acquisition is refused
	// with it rather than re-pinned (#6130).
	retargeted error
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

// confirmSiblingDir verifies the pinned directory holds the legacy config.json
// readSibling is about to open — the premise the conversion's
// readSibling/renameSibling pair depends on. The lock covers config.toml, and
// the conversion reads and renames the legacy config.json BESIDE it through that
// same pinned fd; that is correct only while the config.json readSibling reaches
// is the user's legacy one.
//
// A config.toml installed as a symlink to another directory between LoadConfig
// observing it missing and this lock pinning it leaves dirFD pointing at the
// symlink target's directory rather than the home holding the legacy
// config.json. readSibling would then open an unrelated file there (or, absent
// one, return ENOENT and silently leave the real legacy configuration
// unconverted) — the sibling race the pinned directory exists to close, reopened
// at the file instead of the home. Refuse that one-shot race up front by
// comparing the file the pinned name reaches with the file the legacy path
// resolves to. A config.json that is ITSELF a symlink into the home from
// elsewhere is a valid sibling — readSibling follows it to the same file the
// path resolves to — so this confirms it rather than refusing. A config.json
// that has already vanished is readSibling's to answer with defaults, not a
// symlink race; a home link that moves mid-operation is confirm's, before the
// write and the rename.
func (t lockedTarget) confirmSiblingDir(siblingPath string) error {
	var want unix.Stat_t
	if err := unix.Stat(siblingPath, &want); err != nil {
		if os.IsNotExist(err) {
			// The vanished-sibling case is readSibling's to answer (defaults),
			// not a freshly introduced symlink race — but only when the
			// directory siblingPath reaches is the one the lock covers. A
			// config.json removed after LoadConfig read it while another
			// process installs config.toml as a symlink to a different
			// directory leaves dirFD pointing at that target dir, where
			// readSibling would open an unrelated config.json and the rename
			// would move it aside. Confirm the parent identity the same way
			// the present-sibling and pinned-ENOENT branches below do, and
			// refuse the same one-shot symlink race up front when the
			// directories differ; let the pinned read handle absence only
			// when the legacy path's parent is the pinned directory.
			same, dErr := t.reachesPinnedDir(siblingPath)
			if dErr != nil {
				return dErr
			}
			if !same {
				return fmt.Errorf("cannot convert %s under the lock on %s: the legacy config.json is not in the directory the lock covers (%s) — a freshly introduced config.toml symlink points elsewhere than the home holding it; re-run once config.toml is not a symlink out of its home",
					siblingPath, t.link, t.dirResolved)
			}
			return nil
		}
		return fmt.Errorf("failed to inspect the legacy %s: %w", siblingPath, err)
	}
	if confirmSiblingDirStatHookForTest != nil {
		confirmSiblingDirStatHookForTest()
	}
	// Fstatat with flags=0 follows a symlink the way readSibling's openat does,
	// so the ENOENT check confirms whether the legacy name is in the pinned
	// directory at all. An ENOENT here can mean one of two things: the pinned
	// directory is not the one siblingPath reaches (the TOML-symlink race),
	// or the sibling was concurrently removed between the stat above and this
	// lookup. The latter is readSibling's to answer with defaults, not a
	// refusal that aborts startup, so classify by the parent-directory
	// identity: refuse only when the directories differ; when they match, let
	// the pinned read handle absence the way a vanished sibling already does.
	var got unix.Stat_t
	if err := unix.Fstatat(t.dirFD, filepath.Base(siblingPath), &got, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			same, dErr := t.reachesPinnedDir(siblingPath)
			if dErr != nil {
				return dErr
			}
			if same {
				// The sibling was removed in the same directory the lock covers
				// between the stat above and this lookup; readSibling answers
				// that with defaults rather than aborting startup.
				return nil
			}
			return fmt.Errorf("cannot convert %s under the lock on %s: the legacy config.json is not in the directory the lock covers (%s) — a freshly introduced config.toml symlink points elsewhere than the home holding it; re-run once config.toml is not a symlink out of its home",
				siblingPath, t.link, t.dirResolved)
		}
		return fmt.Errorf("failed to confirm the legacy %s sits beside %s: %w", siblingPath, t.link, err)
	}
	// The file is present in the pinned directory. Compare the directory
	// siblingPath reaches with dirFD rather than the file's own inode, so an
	// editor that atomically replaces config.json (same directory, different
	// inode) is accepted rather than tripping a false out-of-home-symlink
	// refusal. The file-level stat above only confirms the name exists in the
	// pinned directory; the inode can change between that and the directory
	// check below when an editor rewrites config.json in place.
	same, dErr := t.reachesPinnedDir(siblingPath)
	if dErr != nil {
		return dErr
	}
	if same {
		return nil
	}
	return fmt.Errorf("cannot convert %s under the lock on %s: the config.json in the directory the lock covers (%s) is not the legacy %s — a freshly introduced config.toml symlink points at a directory other than the one holding the legacy config.json; re-run once config.toml is not a symlink out of its home",
		siblingPath, t.link, t.dirResolved, siblingPath)
}

// reachesPinnedDir reports whether the directory siblingPath reaches is the
// one the lock covers. confirmSiblingDir uses it both to confirm a present
// sibling sits beside the locked file and to classify an ENOENT from the
// pinned lookup: a same-directory ENOENT is a concurrently removed sibling
// (readSibling answers with defaults), while a different-directory ENOENT is
// the freshly introduced TOML-symlink race.
func (t lockedTarget) reachesPinnedDir(siblingPath string) (bool, error) {
	var wantDir, gotDir unix.Stat_t
	if err := unix.Stat(filepath.Dir(siblingPath), &wantDir); err != nil {
		return false, fmt.Errorf("failed to inspect the directory holding the legacy %s: %w", siblingPath, err)
	}
	if err := unix.Fstat(t.dirFD, &gotDir); err != nil {
		return false, fmt.Errorf("failed to confirm the directory the lock covers for %s: %w", t.link, err)
	}
	return sameInode(&wantDir, &gotDir), nil
}

// confirmSiblingDirStatHookForTest, when non-nil, runs between the
// unix.Stat(siblingPath) lookup and the Fstatat/directory comparison below —
// the check-then-act window an editor's atomic config.json rewrite (same
// directory, different inode) slips into. Tests use it to replace config.json
// in that window and pin the directory-level comparison's acceptance.
var confirmSiblingDirStatHookForTest func()

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

// readSibling reads a file BESIDE the locked one — the legacy config.json the
// one-time json→toml conversion reads — opened through the pinned directory
// rather than by name. It is the read half of the hazard writeSibling and
// removeSibling close for the write and delete: a path opened by name re-resolves
// the home link at open time, so a home that moved between the lock's confirm and
// the body's read feeds the conversion bytes from a directory this lock does not
// cover — the same sibling race migrate.go pins its own body against, left
// path-based here when the conversion was first written and never converted
// (#3688, #3697).
//
// Errors are wrapped as *os.PathError naming path so callers keep testing them
// with os.IsNotExist, the way read does for the locked file itself.
func (t lockedTarget) readSibling(path string) ([]byte, error) {
	fd, err := unix.Openat(t.dirFD, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: path, Err: err}
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

// renameSibling renames a sibling of the locked file — the conversion's
// config.json → config.json.bak move — inside the pinned directory. A
// path-based os.Rename resolves both ends of the move through the home link at
// call time, so a home repointed between the locked config.toml write and this
// move can land it in a directory the lock does not cover — silently renaming
// another home's active config.json to a .bak slot there while the conversion
// reports success. renameat(dirFD, …, dirFD, …) asks the directory this lock
// holds open, the way writeSibling's write and removeSibling's unlink do
// (#3688, #3697). The caller confirms the link still names the pinned
// directory before calling, so a retarget is refused loudly rather than made in
// the wrong place; the move itself does not confirm, mirroring the writers,
// because the fd it uses cannot be redirected regardless.
func (t lockedTarget) renameSibling(srcPath, dstPath string) error {
	if err := unix.Renameat(t.dirFD, filepath.Base(srcPath), t.dirFD, filepath.Base(dstPath)); err != nil {
		return &os.LinkError{Op: "rename", Old: srcPath, New: dstPath, Err: err}
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
	locked, _, err := pinFollowedTarget(path)
	if err != nil {
		return err
	}
	// Close whatever fd the target holds when the body returns, not the one it was
	// first pinned with: acquiring the lock may re-pin a replaced directory.
	defer func() { _ = unix.Close(locked.dirFD) }()
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
func (t *lockedTarget) withLock(fn func() error) error {
	name := t.name + ".lock"
	// Named by path in messages, because that is what the user can go look at.
	lockPath := t.file + ".lock"

	for {
		fd, err := openLockWithRetry(lockPath, func() (int, error) {
			return unix.Openat(t.dirFD, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0644)
		}, t.repinIfReplaced)
		if err != nil {
			if t.retargeted != nil {
				return t.retargeted
			}
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

	f, err := openLockWithRetry(lockPath, func() (*os.File, error) { return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644) }, nil)
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
		f, err := openLockWithRetry(lockPath, func() (*os.File, error) { return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644) }, nil)
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

// lockOpenAttempts and lockOpenBackoff bound openLockWithRetry. Vars so tests
// can shorten them; production never reassigns.
var (
	lockOpenAttempts = 50
	lockOpenBackoff  = 2 * time.Millisecond
)

// openLockWithRetry opens a lock file with O_CREAT and rides out a transient
// ENOENT or EINTR. The lock file is never unlinked by any config writer, and the
// directory is created or pinned before the open, so an O_CREAT open that reports
// "no such file or directory" under contention is either the filesystem losing a
// lookup/create race (seen on macOS, run 40931667845) or a pinned directory that
// was replaced under us, through which openat(O_CREAT) reports ENOENT forever.
// Acquiring the lock must never fail because of another writer, so the open is
// retried a bounded number of times; a directory that is genuinely gone still
// fails, with the original error, after the budget.
//
// onENOENT, when non-nil, runs before each retried ENOENT and returns what it
// observed (and did) as one line of evidence. One warning per retry episode
// carries the last evidence and the attempt count, so a sighting is diagnosable
// from the log alone.
func openLockWithRetry[T any](lockPath string, open func() (T, error), onENOENT func() string) (T, error) {
	var zero T
	var err error
	retries := 0
	evidence := ""
	report := func(outcome string) {
		if retries > 0 {
			log.WarningLog.Printf("lock open %s: %d retr%s after a transient error (%v), %s; %s",
				lockPath, retries, map[bool]string{true: "y", false: "ies"}[retries == 1], err, outcome, evidence)
		}
	}
	for attempt := 0; attempt < lockOpenAttempts; attempt++ {
		var v T
		v, err = open()
		if err == nil {
			report("recovered")
			return v, nil
		}
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.EINTR) {
			report("gave up on a non-retryable error")
			return zero, err
		}
		if errors.Is(err, syscall.ENOENT) && onENOENT != nil {
			evidence = onENOENT()
		}
		retries++
		time.Sleep(lockOpenBackoff)
	}
	report("budget exhausted")
	return zero, err
}

// repinIfReplaced is openLockWithRetry's answer to an ENOENT through the pinned
// directory fd. It compares the fd's (dev, inode) with stat(dir): equal means the
// directory is the one we hold and the ENOENT is the filesystem's race, so the
// caller just retries; different means the directory was replaced (a dotfiles
// `stow -D && stow`, a restored home), and openat(O_CREAT) through the stale fd
// would report ENOENT forever, so the target is re-pinned from the path and the
// stale fd closed. Nothing has been read under the lock yet, and the caller's
// confirm() still runs once the lock is held. The re-pin happens only when the
// path still resolves to the destination pinned at the start; a link that now
// resolves elsewhere is recorded in t.retargeted and the acquisition refused. The
// returned string is the evidence for the retry-episode warning.
func (t *lockedTarget) repinIfReplaced() string {
	var pinned, now unix.Stat_t
	fdErr := unix.Fstat(t.dirFD, &pinned)
	pathErr := unix.Stat(t.dir, &now)
	switch {
	case fdErr != nil || pathErr != nil:
		return fmt.Sprintf("fd identity unavailable (fstat: %v), path identity unavailable (stat %s: %v)", fdErr, t.dir, pathErr)
	case sameInode(&pinned, &now):
		return fmt.Sprintf("fd dev=%d ino=%d nlink=%d matches %s dev=%d ino=%d: the directory is the one pinned",
			pinned.Dev, pinned.Ino, pinned.Nlink, t.dir, now.Dev, now.Ino)
	}
	// Re-pin only a directory replaced at the SAME resolved destination. A link
	// that now resolves elsewhere is a retarget, not a replacement: adopting it
	// would make confirmDir compare the new directory with itself (#6130).
	resolved, evalErr := filepath.EvalSymlinks(t.dir)
	if evalErr != nil || resolved != t.dirResolved {
		t.retargeted = fmt.Errorf("the directory holding %s moved while af was working on it: %s now reaches %s, "+
			"but af pinned %s — nothing was written; re-run the command (resolve error: %v)",
			t.link, t.dir, resolved, t.dirResolved, evalErr)
		return fmt.Sprintf("fd dev=%d ino=%d differs from %s dev=%d ino=%d: the path now resolves to %q, not the pinned %q; refusing to re-pin",
			pinned.Dev, pinned.Ino, t.dir, now.Dev, now.Ino, resolved, t.dirResolved)
	}
	fresh, err := openPinnedDir(t.dir)
	if err != nil {
		return fmt.Sprintf("fd dev=%d ino=%d nlink=%d differs from %s dev=%d ino=%d: the directory was replaced, re-pin failed: %v",
			pinned.Dev, pinned.Ino, pinned.Nlink, t.dir, now.Dev, now.Ino, err)
	}
	stale := t.dirFD
	t.dirFD = fresh
	_ = unix.Close(stale)
	return fmt.Sprintf("fd dev=%d ino=%d nlink=%d differs from %s dev=%d ino=%d: the directory was replaced, re-pinned from the path",
		pinned.Dev, pinned.Ino, pinned.Nlink, t.dir, now.Dev, now.Ino)
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
		f, err := openLockWithRetry(lockPath, func() (*os.File, error) { return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644) }, nil)
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

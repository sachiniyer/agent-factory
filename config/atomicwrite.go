package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// Atomic writes and af's symlink policy, split out of filelock.go when the two
// halves together crossed the 1000-line limit (#3735). The cut is along the seam
// that was already there: filelock.go holds the locking primitives — the
// lockedTarget handle, the pinned-resolution follower, and the WithFileLock
// family — and this file holds what callers do INSIDE a lock, plus the
// per-caller link policy (#3672) those writers implement.
//
// The `atomicwrite_*_test.go` files were named for this file before it existed.

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
// TWO callers, and neither has a followed LOCK to pin its resolution:
//
//   - task/'s tasks.json writer (#3672), the store's ordinary writes;
//   - this package's schema-migration write-back (writeMigratedSchemaFile), for
//     a plan whose LinkPolicy is SchemaWriteFollowLink — today tasks.json again,
//     because a migration must give the same answer about a file as the writes
//     around it (#3718).
//
// Since #3688 every followed write to the GLOBAL CONFIG happens under
// withFollowedFileLock and goes through the resolution that lock pinned, and
// #3697 went further and pinned the DIRECTORY the lock was taken in, because
// resolving again inside a critical section is the bug those issues are about.
// withFollowedFileLock and lockedTarget are unexported on purpose, so neither
// caller above can reach that machinery — task/ cannot by package, and the
// write-back must not, since its lock is bounded where that one blocks forever.
//
// So both leave tasks.json with the #3688 shape a link retargeted MID-OPERATION
// can still catch: the read resolves at open time and the write resolves again.
// #3697 closed that for the global config's followed lock, not for a followed
// write with no lock to pin one; #3672 and #3718 deliberately left it alone, and
// writeMigratedSchemaFile records why the migration takes the same unresolved
// lock the ordinary writes do. It is not the #3660 bug: an unmoving link is
// followed correctly, which is the promise tasks.json was put on this side for.
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
//
// It is the seam EVERY atomic write and every file lock passes through, which is
// why the #3845 precondition sits here: a daemon whose home was deleted used to
// re-create it on its next state write, because MkdirAll creates the home as an
// ancestor of whatever it was asked for. The check is FIRST — ahead of
// secureAFHomeForPath, which has an os.MkdirAll of the home of its own — so a
// refused write leaves the filesystem exactly as it found it.
func ensureStorageParent(path string) error {
	if err := requireObservedAFHomePresent(path); err != nil {
		return err
	}
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

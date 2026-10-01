package git

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/log"
)

// Extended attributes on the cross-device worktree copy (#2919).
//
// rename(2) carries them for free; the copy path had to reproduce them, and nothing
// did — `grep -rn "Getxattr|Setxattr|Listxattr"` over the repo returned nothing, so
// capabilities, SELinux labels and ACLs were all dropped by an archive that reported
// success. Split out of worktree_copy_tree.go to keep that file under the length
// limit; the ordering constraints that tie this to the mode live at the call sites.

// copySourceXattrs reproduces a source node's extended attributes on its copy.
//
// rename(2) keeps them for free. The copy reproduced only what it was written to
// reproduce, so every namespace was dropped and which filesystem $AF_HOME sits on
// decided whether a restored worktree kept them (#2919). What is lost is not
// decoration: system.posix_acl_access / _default carry named-user and named-group
// grants, and a DIRECTORY's default ACL vanishing is the surprising direction —
// files created in the restored tree afterwards inherit plain umask permissions,
// which can be WIDER than the policy that was archived. security.capability turns a
// vendored helper into one that silently cannot bind; security.selinux mislabels a
// tree on an enforcing box.
//
// Descriptor-anchored like every other step here: the F* forms take the descriptors
// the walk already validated, so no path is re-derived and the name-swap race the
// copier exists to avoid stays avoided.
//
// The failure policy is per-attribute and deliberately asymmetric, because "the
// archive refused to run" is a worse outcome than "one label could not be stored" —
// but only while the loss is LOGGED rather than silent, which is the actual
// complaint in #2919:
//
//   - the destination filesystem holds no xattrs at all -> warn once and stop trying.
//     The archive root's filesystem is not a per-file choice. Proven by asking the
//     destination descriptor, never inferred from one rejected name: EOPNOTSUPP also
//     comes back per NAMESPACE, and a destination that stores user.* fine can still
//     refuse a security.* attribute.
//   - one attribute is refused (EPERM/EACCES - security.capability needs CAP_SETFCAP,
//     security.selinux needs relabel permission; or a namespace this filesystem does
//     not implement) -> warn naming it, keep going.
//   - anything else -> fail the copy. E2BIG and ENOSPC are errors, not policy limits.
//
// trusted.* needs no special case: an unprivileged lister never sees it, so it never
// appears in the list to attempt.
// maxXattrValueBytes caps one attribute's value. Real metadata — capabilities,
// SELinux labels, ACLs, user tags — is tens to hundreds of bytes; this is orders of
// magnitude above that while still bounding a single allocation.
const maxXattrValueBytes = 1 << 20

// errXattrValueTooLarge marks a value the copier declines to buffer.
var errXattrValueTooLarge = errors.New("extended attribute value exceeds the copy limit")

// errXattrUnsupportedDestination marks a destination FILESYSTEM that holds no
// attributes at all. It is a property of the archive root, not of one node, so the
// caller stops attempting the rest of the tree rather than re-learning it per file.
var errXattrUnsupportedDestination = errors.New("destination filesystem does not support extended attributes")

// errXattrPathTooLong tells copySymlinkEntry to skip the path-based prune and route
// recheck for a link. Its original meaning is a route the path-based L* xattr family
// cannot address because the textual path exceeds the kernel's PATH_MAX: the
// descriptor-anchored F* family the file and directory paths use has no such limit
// (it takes an fd), but the symlink path has no *at variant in golang.org/x/sys/unix
// (v0.47.0), so a tree the copier reaches component-by-component through directory
// descriptors can carry a link whose route is too long for L* even though the walker
// itself copied it. The sentinel is also returned when the filesystem-wide latch
// (holdsNone, set by a file/dir copy that proved the destination holds no xattrs at
// all) is already set: no L* call ran through the path, so there is nothing for the
// route recheck to verify and nothing for the prune to remove, and a link whose
// route exceeds PATH_MAX would abort the recheck's Lstat even though the latch
// already settled the question. It is non-fatal — the #2919 invariant the file and
// directory paths follow is "log the loss, do not abort the archive" — so the caller
// skips the link's xattr copy, prune, and route recheck and continues, rather than
// aborting a cross-device move of a tree the descriptor-anchored walker copied.
var errXattrPathTooLong = errors.New("symlink path too long for the L* xattr family")

func copySourceXattrs(sourceFD, destinationFD int, destinationPath, kind string, acl bool) error {
	names, err := listXattrNames(sourceFD)
	if err != nil {
		if isXattrUnsupported(err) {
			return nil // the SOURCE filesystem has no xattrs; nothing to carry
		}
		return fmt.Errorf(
			"cannot move worktree across filesystems: failed to list extended attributes for destination %s %s: %w",
			kind, destinationPath, err,
		)
	}
	for _, name := range names {
		if isACLXattr(name) != acl {
			continue // the other phase owns this one
		}
		value, err := readXattrValue(sourceFD, name)
		if err != nil {
			if isXattrVanished(err) {
				// Removed between the listing and the read. The only tolerable read
				// failure, and warned so it is not silent.
				log.WarningLog.Printf(
					"archive: extended attribute %q vanished from %s %s while it was being copied",
					name, kind, destinationPath,
				)
				continue
			}
			if errors.Is(err, errXattrValueTooLarge) {
				// A value too large to buffer — on darwin com.apple.ResourceFork can be
				// fork-sized. Skipping one attribute is survivable; allocating it is not,
				// and an archive that OOMs takes the session with it.
				log.WarningLog.Printf(
					"archive: extended attribute %q on %s %s is too large to copy (%d byte limit); not reproduced",
					name, kind, destinationPath, maxXattrValueBytes,
				)
				continue
			}
			// Anything else — EIO from a network filesystem, a transient resource
			// failure — is a real read error. Reporting success while dropping a
			// capability or a label is the silent-loss this issue is about.
			return fmt.Errorf(
				"cannot move worktree across filesystems: failed to read extended attribute %q from %s %s: %w",
				name, kind, destinationPath, err,
			)
		}
		if err := unix.Fsetxattr(destinationFD, name, value, 0); err != nil {
			switch {
			case isXattrUnsupported(err):
				// EOPNOTSUPP here is per-NAME, not per-filesystem: a destination that
				// stores user.* happily can still reject a namespace it does not
				// implement. Latching on the first one would skip every remaining
				// attribute on this node AND on every later node, so the filesystem-wide
				// claim has to be established independently before it is believed.
				if !destinationRejectsAllXattrs(destinationFD) {
					log.WarningLog.Printf(
						"archive: extended attribute %q not reproduced on %s %s (this destination does not implement that namespace): %v",
						name, kind, destinationPath, err,
					)
					continue
				}
				log.WarningLog.Printf(
					"archive: %s %s cannot hold extended attributes on this filesystem; none were copied",
					kind, destinationPath,
				)
				return errXattrUnsupportedDestination
			case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
				log.WarningLog.Printf(
					"archive: extended attribute %q not reproduced on %s %s (needs privilege): %v",
					name, kind, destinationPath, err,
				)
			default:
				return fmt.Errorf(
					"cannot move worktree across filesystems: failed to set extended attribute %q on destination %s %s: %w",
					name, kind, destinationPath, err,
				)
			}
		}
	}
	return nil
}

// xattrDestination latches the discovery that a destination holds no extended
// attributes at all, so the rest of THAT copy stops attempting them instead of
// logging the same warning once per file in the worktree.
//
// One per copy, like the hardlink map in copyDirectoryContents and for the same
// reason. A process-wide latch would be wrong rather than merely untidy: this
// copier also serves RestoreWorktreeTo, whose destination is an arbitrary
// repository filesystem rather than the archive root, so a single restore onto a
// filesystem without xattr support would silently disable attribute copying for
// every later archive in the daemon's lifetime — and the daemon is long-lived.
//
// Not atomic: a copy walks its tree on one goroutine, and the value never escapes
// the copy that made it.
type xattrDestination struct {
	holdsNone bool
}

// copyNonACLXattrs and copyACLXattrs are the two halves of the copy, split around
// the mode — see isACLXattr for why the split is load-bearing.
func copyNonACLXattrs(support *xattrDestination, sourceFD, destinationFD int, destinationPath, kind string) error {
	return copyXattrPhase(support, sourceFD, destinationFD, destinationPath, kind, false)
}

func copyACLXattrs(support *xattrDestination, sourceFD, destinationFD int, destinationPath, kind string) error {
	return copyXattrPhase(support, sourceFD, destinationFD, destinationPath, kind, true)
}

func copyXattrPhase(support *xattrDestination, sourceFD, destinationFD int, destinationPath, kind string, acl bool) error {
	if support.holdsNone {
		return nil
	}
	err := copySourceXattrs(sourceFD, destinationFD, destinationPath, kind, acl)
	if errors.Is(err, errXattrUnsupportedDestination) {
		support.holdsNone = true
		return nil
	}
	return err
}

// copySymlinkXattrs is the symlink analogue of copyNonACLXattrs/copyACLXattrs. A
// symlink yields no descriptor the F* xattr syscalls can target — O_NOFOLLOW on a
// link returns ELOOP, and the only fd a symlink gives is O_PATH|O_NOFOLLOW, which
// makes Flistxattr/Fgetxattr return EBADF — so the descriptor-anchored
// copySourceXattrs cannot reach a link's own attributes. The path-based L* family
// (Llistxattr/Lgetxattr/Lsetxattr) does not follow the link, so it addresses the
// link itself; there is no *at xattr variant in golang.org/x/sys/unix (v0.47.0) to
// match the UtimesNanoAt trick the mtime stamp relies on, so the path is the only
// handle.
//
// The per-attribute failure policy mirrors copySourceXattrs, with one
// load-bearing exception: a refused namespace (EPERM/EACCES — on Linux user.*
// cannot be set on a symlink and only security.* / trusted.* live on links in
// production, neither settable without SELinux or root) is LOGGED and skipped
// rather than failing the archive, which is the #2919 invariant the silent drop
// violated; anything else fails the copy. ACLs are not split out:
// system.posix_acl_* does not apply to symlinks, so the file/dir ordering around
// the mode has no analogue here.
//
// The exception is the filesystem-wide latch. copySourceXattrs latches
// holdsNone when Flistxattr on a real descriptor proves the destination FILESYSTEM
// holds no attributes at all, which is correct for a file or directory because the
// probe reaches the filesystem's own xattr code path. A symlink probe cannot make
// that claim: Llistxattr on a link reports EOPNOTSUPP on filesystems that store
// attributes on files and directories fine — the kernel's symlink-xattr handler
// is a separate code path — so latching holdsNone from a symlink would let an
// early link in iteration order silently drop every later file's and directory's
// attributes on a destination that can hold them. The symlink path therefore
// never latches: a destination link that holds no attributes is logged and
// scoped to that link, and the file and directory paths keep sole authority over
// the filesystem-wide latch.
//
// When holdsNone was latched by a file/dir copy, this returns errXattrPathTooLong
// rather than nil. copySymlinkEntry treats the sentinel as "no L* call ran; skip
// the path-based prune and route recheck," which is correct for the latched case:
// the destination holds no attributes so prune has nothing to remove, no L* call
// read or wrote the path so there is nothing for the route recheck to verify, and a
// link whose route exceeds PATH_MAX would abort the recheck's Lstat even though the
// latch already settled the question. Returning nil would run those path-based
// operations and, for a too-long route, abort a cross-device move the
// descriptor-anchored F* paths copy fine.
func copySymlinkXattrs(support *xattrDestination, sourcePath, destinationPath string) error {
	if support.holdsNone {
		// See the doc comment above: no L* call ran (the destination holds no
		// xattrs at all), so the path-based prune and route recheck in
		// copySymlinkEntry must be skipped. A too-long route would otherwise abort
		// the move via the recheck's Lstat, the very thing errXattrPathTooLong
		// exists to prevent.
		return errXattrPathTooLong
	}
	err := copySymlinkSourceXattrs(sourcePath, destinationPath)
	if errors.Is(err, errXattrUnsupportedDestination) {
		// Do NOT latch holdsNone: the EOPNOTSUPP is a property of the link, not
		// the filesystem (see the doc comment above). The warning inside
		// copySymlinkSourceXattrs already recorded the link-local refusal, so
		// this is logged, not silent (#2919).
		return nil
	}
	if errors.Is(err, errXattrPathTooLong) {
		// The link's route exceeds PATH_MAX, so the L* family cannot address it and
		// the descriptor-anchored F* paths cannot reach a symlink either — there is
		// no *at xattr variant. copySymlinkEntry recognizes the sentinel and skips
		// the prune and the route recheck (which Lstat the same too-long path), so
		// the cross-device move continues for a tree the walker already copied. The
		// loss is logged here, not silent (#2919).
		log.WarningLog.Printf(
			"archive: could not reproduce extended attributes on symlink %s because its path is too long for the L* xattr family (no *at variant exists); the attributes are left in place: %v",
			sourcePath, err,
		)
		return err
	}
	return err
}

// copySymlinkSourceXattrs is the path-based twin of copySourceXattrs, reading the
// source link's own attributes with Lgetxattr and writing them to the destination
// link with Lsetxattr. Neither call follows the link.
func copySymlinkSourceXattrs(sourcePath, destinationPath string) error {
	names, err := listSymlinkXattrNames(sourcePath)
	if err != nil {
		if isXattrUnsupported(err) {
			return nil // the source filesystem has no xattrs; nothing to carry
		}
		if errors.Is(err, unix.ENAMETOOLONG) {
			// The L* family has no *at form, so the link's path is the full textual
			// route. A tree the walker reaches component-by-component through directory
			// descriptors can carry a link whose route exceeds PATH_MAX, and Llistxattr
			// then returns ENAMETOOLONG even for a link with no attributes — which would
			// abort a cross-device move the descriptor-anchored F* paths copy fine. There
			// is no descriptor-relative L* variant to fall back to, so the attribute loss
			// is reported and the copy continues, the #2919 invariant the per-attribute
			// refusal follows.
			return errXattrPathTooLong
		}
		return fmt.Errorf(
			"cannot move worktree across filesystems: failed to list extended attributes for destination symlink %s: %w",
			destinationPath, err,
		)
	}
	for _, name := range names {
		value, err := readSymlinkXattrValue(sourcePath, name)
		if err != nil {
			if isXattrVanished(err) {
				log.WarningLog.Printf(
					"archive: extended attribute %q vanished from symlink %s while it was being copied",
					name, destinationPath,
				)
				continue
			}
			if errors.Is(err, errXattrValueTooLarge) {
				log.WarningLog.Printf(
					"archive: extended attribute %q on symlink %s is too large to copy (%d byte limit); not reproduced",
					name, destinationPath, maxXattrValueBytes,
				)
				continue
			}
			if errors.Is(err, unix.ENAMETOOLONG) {
				return errXattrPathTooLong
			}
			return fmt.Errorf(
				"cannot move worktree across filesystems: failed to read extended attribute %q from symlink %s: %w",
				name, destinationPath, err,
			)
		}
		if err := unix.Lsetxattr(destinationPath, name, value, 0); err != nil {
			switch {
			case errors.Is(err, unix.ENAMETOOLONG):
				// The destination route is too long for the L* family for the same
				// reason the source route above can be; see errXattrPathTooLong.
				return errXattrPathTooLong
			case isXattrUnsupported(err):
				if !destinationSymlinkRejectsAllXattrs(destinationPath) {
					log.WarningLog.Printf(
						"archive: extended attribute %q not reproduced on symlink %s (this destination does not implement that namespace): %v",
						name, destinationPath, err,
					)
					continue
				}
				log.WarningLog.Printf(
					"archive: symlink %s cannot hold extended attributes on this filesystem; none were copied",
					destinationPath,
				)
				return errXattrUnsupportedDestination
			case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
				log.WarningLog.Printf(
					"archive: extended attribute %q not reproduced on symlink %s (needs privilege): %v",
					name, destinationPath, err,
				)
			default:
				return fmt.Errorf(
					"cannot move worktree across filesystems: failed to set extended attribute %q on destination symlink %s: %w",
					name, destinationPath, err,
				)
			}
		}
	}
	return nil
}

// listSymlinkXattrNames is the path-based twin of listXattrNames: Llistxattr does not
// follow the link, so it lists the link's own attributes rather than the target's.
func listSymlinkXattrNames(path string) ([]string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		size, err := unix.Llistxattr(path, nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		buffer := make([]byte, size)
		read, err := unix.Llistxattr(path, buffer)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue
			}
			return nil, err
		}
		names := make([]string, 0, 4)
		for _, name := range bytes.Split(buffer[:read], []byte{0}) {
			if len(name) > 0 {
				names = append(names, string(name))
			}
		}
		return names, nil
	}
	return nil, fmt.Errorf("extended attribute list kept growing while it was read")
}

// readSymlinkXattrValue is the path-based twin of readXattrValue: Lgetxattr does not
// follow the link, so it reads the link's own attribute rather than the target's.
func readSymlinkXattrValue(path, name string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		size, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		if size > maxXattrValueBytes {
			return nil, errXattrValueTooLarge
		}
		value := make([]byte, size)
		read, err := unix.Lgetxattr(path, name, value)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue
			}
			return nil, err
		}
		return value[:read], nil
	}
	return nil, fmt.Errorf("extended attribute %q kept growing while it was read", name)
}

// destinationSymlinkRejectsAllXattrs is the path-based twin of
// destinationRejectsAllXattrs: Llistxattr on the freshly created link reports whether
// the destination filesystem holds attributes at all, so a single refused namespace
// is not mistaken for a filesystem that has none.
func destinationSymlinkRejectsAllXattrs(path string) bool {
	_, err := unix.Llistxattr(path, nil)
	return isXattrUnsupported(err)
}

// pruneSymlinkXattrs is the path-based twin of pruneDestinationXattrs: a freshly
// created destination link can carry an attribute the source lacks — an
// SELinux-enabled destination assigns security.selinux during symlinkat while the
// source filesystem is unlabeled, and the file and directory paths remove exactly
// such an inherited attribute via pruneDestinationXattrs. The link path was the
// one node class that did not, so this lists the destination link's own attributes
// with Llistxattr and Lremovexattr's any the source link does not carry. Failures
// are warnings, matching pruneDestinationXattrs: an archive that refuses to run
// is worse than one that reports what it could not normalise.
func pruneSymlinkXattrs(sourcePath, destinationPath string) {
	destinationNames, err := listSymlinkXattrNames(destinationPath)
	if err != nil {
		// A destination that holds no attributes at all has nothing to normalise
		// and is not worth a warning. Anything else means the check did not happen,
		// and staying silent is how an inherited attribute rides along while the
		// archive reports success.
		if !isXattrUnsupported(err) {
			log.WarningLog.Printf(
				"archive: could not list extended attributes on symlink %s to check for inherited ones; any the destination added are left in place: %v",
				destinationPath, err,
			)
		}
		return
	}
	if len(destinationNames) == 0 {
		return
	}
	sourceNames, err := listSymlinkXattrNames(sourcePath)
	if err != nil && !isXattrUnsupported(err) {
		log.WarningLog.Printf(
			"archive: could not list the source's extended attributes while normalising symlink %s; any the destination inherited are left in place: %v",
			destinationPath, err,
		)
		return
	}
	fromSource := make(map[string]struct{}, len(sourceNames))
	for _, name := range sourceNames {
		fromSource[name] = struct{}{}
	}
	for _, name := range destinationNames {
		if _, ok := fromSource[name]; ok {
			continue
		}
		if err := unix.Lremovexattr(destinationPath, name); err != nil && !isXattrVanished(err) {
			log.WarningLog.Printf(
				"archive: symlink %s inherited extended attribute %q that the source did not have, and it could not be removed: %v",
				destinationPath, name, err,
			)
		}
	}
}

// assertPathResolvesToVerifiedLeaf detects that a textual path the L* xattr
// family read or wrote through was diverted from the node a held descriptor
// anchors. The L* family has no *at form in golang.org/x/sys/unix (v0.47.0), so
// copySymlinkXattrs reads sourcePath and writes destinationPath through the
// filesystem root rather than the source/destination descriptors the walk
// validated. A descriptor-anchored recheck (statAt(parent, name)) proves the
// LEAF in the held parent did not change, but it does not prove the textual path
// still resolves through the same ANCESTORS to that leaf: a same-UID process that
// renames an ancestor and installs a replacement at the same name during the L*
// calls reads the replacement's attributes (or writes the source's onto the
// replacement) while the held parent's name still resolves to the original — and
// the final tree validation re-walks by descriptor, so it misses the swap too.
//
// Lstat re-derives the path through its ancestors without following the final
// component, so for a symlink path it addresses the link itself. A leaf the path
// reaches that is not the leaf the held descriptor reached means an ancestor was
// swapped, and the archive refuses rather than publish a transient node's
// attributes. A swap that is restored before this check is the residual the
// *at-less API leaves, the same detection-only standard the mtime stamp in
// copySymlinkEntry already applies; the loss stays LOGGED (#2919) rather than
// silent.
func assertPathResolvesToVerifiedLeaf(path string, verified *unix.Stat_t) error {
	var resolved unix.Stat_t
	if err := unix.Lstat(path, &resolved); err != nil {
		return err
	}
	if !identityFromStat(verified).same(identityFromStat(&resolved)) {
		return fmt.Errorf("path %s resolves to a different node than the verified descriptor", path)
	}
	return nil
}

// pruneDestinationXattrs removes attributes the destination has and the source does
// not.
//
// Copying alone is not fidelity. A destination parent carrying a DEFAULT POSIX ACL
// gives every newly created child a system.posix_acl_access of its own, so a source
// file with no ACL arrives with one — and an inherited ACL is generally WIDER than
// the mode it replaces, which makes this a permission-widening bug rather than an
// untidy one. The same applies to any attribute a destination filesystem or parent
// synthesises.
//
// Failures here are warnings, not errors, for the reason the whole file follows: an
// archive that refuses to run is worse than one that reports what it could not
// normalise.
func pruneDestinationXattrs(sourceFD, destinationFD int, destinationPath, kind string) {
	destinationNames, err := listXattrNames(destinationFD)
	if err != nil {
		// A destination that holds no attributes at all has nothing to normalise and is
		// not worth a warning. Anything else — EIO, ENOMEM — means the check did not
		// happen, and staying silent about that is how an inherited ACL rides along
		// while the archive reports success.
		if !isXattrUnsupported(err) {
			log.WarningLog.Printf(
				"archive: could not list extended attributes on %s %s to check for inherited ones; any the destination added are left in place: %v",
				kind, destinationPath, err,
			)
		}
		return
	}
	if len(destinationNames) == 0 {
		return
	}
	sourceNames, err := listXattrNames(sourceFD)
	if err != nil && !isXattrUnsupported(err) {
		// Cannot tell what the source had, so leave the destination alone rather than
		// remove something the source may well have carried — but say so, because an
		// inherited ACL surviving is the permission-widening case this helper exists
		// to prevent.
		log.WarningLog.Printf(
			"archive: could not list the source's extended attributes while normalising %s %s; any the destination inherited are left in place: %v",
			kind, destinationPath, err,
		)
		return
	}
	fromSource := make(map[string]struct{}, len(sourceNames))
	for _, name := range sourceNames {
		fromSource[name] = struct{}{}
	}
	for _, name := range destinationNames {
		if _, ok := fromSource[name]; ok {
			continue
		}
		if err := unix.Fremovexattr(destinationFD, name); err != nil && !isXattrVanished(err) {
			log.WarningLog.Printf(
				"archive: %s %s inherited extended attribute %q that the source did not have, and it could not be removed: %v",
				kind, destinationPath, name, err,
			)
		}
	}
}

// isACLXattr reports whether a name is a POSIX ACL attribute, which must be applied
// AFTER the mode: setting system.posix_acl_access rewrites the mode, and chmod
// rewrites the ACL mask, so an ACL written before the mode is silently undone.
// Everything else must be applied BEFORE it, because setting an attribute in the
// user namespace requires write permission on the inode.
//
// LINUX ONLY, and the split is named for what it does rather than for a guarantee
// it makes. Darwin does not represent ACLs as extended attributes — they live
// behind acl(3), so these names never appear in a darwin Flistxattr listing, this
// phase is an empty pass there, and a macOS worktree's named-user ACL entries are
// NOT reproduced by the cross-device copy. That is unchanged by #2919 rather than
// introduced by it (nothing carried any attribute before), and closing it needs the
// ACL API, not another name in this predicate. Not in knownCrossDeviceDivergence:
// that inventory is keyed by properties describeFidelity actually measures, and
// measuring this one needs a darwin runner the guard does not have. Tracked in
// #2919 instead, so the limit is written down where the follow-up will look.
func isACLXattr(name string) bool {
	return name == "system.posix_acl_access" || name == "system.posix_acl_default"
}

// destinationRejectsAllXattrs reports whether a destination holds no extended
// attributes AT ALL, as opposed to having refused one particular name.
//
// Asked of the destination descriptor itself, because that is the only thing that
// can answer it: a listing that comes back unsupported means the filesystem does not
// implement xattrs, while a listing that succeeds proves it does and that the refusal
// was about the one namespace. Without this the first rejected security.* attribute
// would convince the copier that the whole filesystem was attribute-less and make it
// skip the user.* and ACL attributes it could have stored.
func destinationRejectsAllXattrs(destinationFD int) bool {
	_, err := unix.Flistxattr(destinationFD, nil)
	return isXattrUnsupported(err)
}

// isXattrUnsupported reports whether an error means extended attributes are not
// available here at all. Platform-split for the same reason as isXattrVanished, and
// with a sharper consequence: darwin spells this ENOTSUP (0x2d) where Linux returns
// EOPNOTSUPP (0x5f) and makes the two names one value, so matching only EOPNOTSUPP
// would send every unsupported macOS destination down the "unexpected error" path and
// fail the archive outright instead of degrading to a warning.
func isXattrUnsupported(err error) bool {
	for _, unsupported := range xattrUnsupportedErrnos() {
		if errors.Is(err, unsupported) {
			return true
		}
	}
	return false
}

// isXattrVanished reports whether an error means the attribute is simply not there —
// removed between the listing and the read, or never present.
//
// This is the one place a platform split is unavoidable: Linux reports ENODATA and
// darwin reports ENOATTR, they are DIFFERENT numbers, and unix.ENOATTR does not exist
// on Linux at all, so naming both in one file does not compile. The distinction has
// to be drawn because "the attribute vanished" is the only read failure this copier
// may tolerate — everything else (EIO from a network filesystem, a resource failure)
// must fail the archive rather than silently drop a capability or a label.
func isXattrVanished(err error) bool {
	for _, absent := range xattrAbsentErrnos() {
		if errors.Is(err, absent) {
			return true
		}
	}
	return false
}

// listXattrNames returns the attribute names visible on fd. Sized in two calls
// because the set can change between them, so a list that grew is read short and
// retried rather than silently truncated.
func listXattrNames(fd int) ([]string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		size, err := unix.Flistxattr(fd, nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		buffer := make([]byte, size)
		read, err := unix.Flistxattr(fd, buffer)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue // the list grew between the sizing and the read
			}
			return nil, err
		}
		names := make([]string, 0, 4)
		for _, name := range bytes.Split(buffer[:read], []byte{0}) {
			if len(name) > 0 {
				names = append(names, string(name))
			}
		}
		return names, nil
	}
	return nil, fmt.Errorf("extended attribute list kept growing while it was read")
}

// readXattrValue reads one attribute's value, sized the same two-call way. A present
// attribute with an empty value is meaningful, so nil-with-no-error is a real result.
func readXattrValue(fd int, name string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		if size > maxXattrValueBytes {
			// darwin exposes com.apple.ResourceFork through this API and its value can
			// be as large as a regular fork, so a size returned here is not bounded by
			// "metadata". Allocating it would let one file's resource fork exhaust the
			// daemon's memory mid-archive.
			return nil, errXattrValueTooLarge
		}
		value := make([]byte, size)
		read, err := unix.Fgetxattr(fd, name, value)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue // the value grew between the sizing and the read
			}
			return nil, err
		}
		return value[:read], nil
	}
	return nil, fmt.Errorf("extended attribute %q kept growing while it was read", name)
}

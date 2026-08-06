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

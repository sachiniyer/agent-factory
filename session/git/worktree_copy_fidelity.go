// Property-fidelity primitives for the cross-device tree copier, split from
// worktree_copy_tree.go when that file reached the 1000-line limit (#1145).
//
// rename(2) carries every filesystem property for free; the copy path has to
// reproduce each one by hand, so this file holds the enumeration: the working
// modes a node is created writable with, the real mode and modification time a
// finished copy is stamped with, and the hole-preserving byte copy that keeps
// a sparse file sparse. worktree_copy_fidelity_test.go is the differential
// guard that fails when moveDirCrossDevice's two paths diverge on a property
// not in its denylist (#2919).
package git

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// workingDirectoryMode is the mode a destination directory is created with while
// the copy is still filling it.
//
// A worktree may legitimately contain a directory its owner cannot write — a
// vendored or generated tree checked in read-only. rename(2) relocates one
// without ever looking inside, but a copy has to put the contents back, and
// creating the directory at its final 0555 makes it impossible to fill: the
// archive failed outright on a worktree the same-device path moves without
// complaint (#2872). So the copy runs in a mode that guarantees it can write,
// and applyCopiedDirectoryMode installs the real one the moment the directory's
// own level is complete.
//
// Only owner bits are added. No other user gains access to the staging tree at
// any point, whatever the source's mode says.
func workingDirectoryMode(sourceMode os.FileMode) uint32 {
	return uint32(sourceMode.Perm()) | 0o700
}

// workingFileMode is the mode a destination FILE is created with while the copy
// is still filling it, and it exists for the same reason as
// workingDirectoryMode.
//
// Creating the file at its final mode makes a read-only source file impossible
// to finish: setting an attribute in the user namespace requires write
// permission on the INODE, and an already-open write descriptor does not supply
// it, so on a checked-in 0444 file every user.* attribute failed EACCES no
// matter where in the sequence it was attempted. Ordering alone cannot fix that
// — the inode is read-only from the moment openat() creates it — so the copy
// runs at a mode that guarantees it can write and preserveSourceMode installs
// the real one once the attributes are on.
//
// Only owner bits are added, exactly as for directories: no other user gains
// access to the staging file at any point, whatever the source's mode says.
func workingFileMode(sourceMode os.FileMode) uint32 {
	return uint32(sourceMode.Perm()) | 0o600
}

// applyCopiedDirectoryMode gives a finished destination directory the mode its
// source carries, reading that mode from the source descriptor the route walk
// just validated rather than from anything cached earlier.
func applyCopiedDirectoryMode(source, destination *os.File, destinationPath string, support *xattrDestination) error {
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf(
			"cannot move worktree across filesystems: failed to read the source mode for destination directory %s: %w",
			destinationPath, err,
		)
	}
	// Attributes split around the mode, and neither half is arbitrary.
	//
	// NON-ACL first: setting an attribute in the user namespace requires write
	// permission on the inode, and the mode applied below may remove it (a 0444 or
	// 0555 source), so a user.* attribute written afterwards fails EACCES — and the
	// privilege branch would then log it as "needs privilege" and drop it, silently
	// losing an ordinary attribute on every read-only node.
	//
	// ACL last: the access ACL and the mode bits are one state seen two ways. Setting
	// system.posix_acl_access rewrites the mode, and chmod rewrites the ACL mask, so
	// an ACL applied before the mode would be silently undone.
	//
	// setxattr moves ctime only, so the times applied below still land last (#2919).
	if err := copyNonACLXattrs(support, int(source.Fd()), int(destination.Fd()), destinationPath, "directory"); err != nil {
		return err
	}
	// While the directory is still writable: removing an inherited attribute needs
	// write permission just as setting one does, and the mode below may take it away.
	pruneDestinationXattrs(int(source.Fd()), int(destination.Fd()), destinationPath, "directory")
	if err := preserveSourceMode(int(destination.Fd()), info.Mode(), destinationPath, "directory"); err != nil {
		return err
	}
	if err := copyACLXattrs(support, int(source.Fd()), int(destination.Fd()), destinationPath, "directory"); err != nil {
		return err
	}
	// Times last, and only here. Creating an entry inside a directory updates
	// that directory's own mtime, so a timestamp written any earlier would be
	// overwritten by the copy's own writes — the same ordering the mode already
	// depends on.
	return preserveSourceModTime(int(destination.Fd()), ".", info.ModTime(), destinationPath, "directory")
}

// holeCopyChunk is the read granularity of the hole-preserving copy. It is a
// multiple of every common filesystem block size, so a skipped chunk lands on
// block boundaries and actually leaves a hole rather than a partly-allocated
// extent.
const holeCopyChunk = 256 << 10

// copyContentsPreservingHoles copies in to out without writing runs of zeros, so
// a sparse file arrives sparse instead of fully allocated.
//
// io.Copy reads a hole as zeros and faithfully writes them out, which allocates
// a real block for every hole: a 4 KiB-on-disk, 64 MiB-long file arrived in the
// archive as 64 MiB of blocks, a 16384x amplification (#2920). rename(2) never
// reads the file, so the same-device path keeps the extent layout untouched and
// only the cross-device path pays this. The archive root is shared by every
// session on the box, so one session archiving a sparsely-allocated database or
// image file can consume the space all of them depend on.
//
// A hole and a run of zeros are indistinguishable to every reader, so skipping
// the write cannot change what anyone sees — it changes only what is allocated.
// This is cp --sparse=always semantics: holes are reproduced at write
// granularity rather than exactly, and a run of explicit zeros may become a
// hole.
//
// Deliberately not SEEK_DATA/SEEK_HOLE extent iteration. That reproduces the
// layout more exactly, but it replaces the read loop in the one function that
// must never lose worktree bytes, and it degrades differently per filesystem.
// This keeps the read-to-EOF loop and its behavior on a concurrently written
// file, and changes only whether a zero-filled buffer is written or skipped.
func copyContentsPreservingHoles(out, in *os.File) error {
	buffer := make([]byte, holeCopyChunk)
	var copied int64
	for {
		// ReadFull, not Read: a short read mid-file would push every later chunk
		// off its block boundary, and a misaligned skip allocates instead of
		// leaving a hole. Only the final chunk is allowed to be short.
		n, err := io.ReadFull(in, buffer)
		if n > 0 {
			if !isAllZero(buffer[:n]) {
				if _, writeErr := out.WriteAt(buffer[:n], copied); writeErr != nil {
					return writeErr
				}
			}
			copied += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	// Load-bearing, not tidiness: a file whose final chunk was skipped has had
	// nothing written at that offset, so without this the copy is short by the
	// whole trailing hole.
	return out.Truncate(copied)
}

func isAllZero(chunk []byte) bool {
	for _, b := range chunk {
		if b != 0 {
			return false
		}
	}
	return true
}

// preserveSourceModTime reproduces a source node's modification time on its copy.
//
// The copy writes new nodes, so without this every file and directory in an
// archived worktree carries the time it was archived rather than the time its
// contents were last touched. rename(2) keeps them exactly, so this is one more
// place where which filesystem $AF_HOME sits on decided what a restored session
// looked like (#2919). It is not cosmetic: build tools decide what to rebuild
// from mtime, and git's index stores stat data, so a restored worktree that
// looks entirely new re-hashes every file it could have trusted.
//
// Anchored to a directory descriptor plus a name, like the node creation around
// it, with AT_SYMLINK_NOFOLLOW so a name swapped for a symlink between creation
// and this call cannot redirect the timestamp onto a file outside the tree. A
// DIRECTORY passes its own descriptor with "." — that addresses the directory
// itself without AT_EMPTY_PATH, which is Linux-only, so one call covers every
// platform at full nanosecond resolution.
//
// Both slots are set to the source mtime because there is no portable way to set
// one and leave the other: UTIME_OMIT is not defined on darwin. atime is
// therefore NOT preserved, deliberately — it is rewritten by any read, most
// filesystems mount relatime so it is already approximate, and nothing in a
// worktree depends on it. mtime is the property tools actually read.
// symlinkModTime reads a link's own modification time from the lstat that
// identified it.
//
// No build tag, deliberately: x/sys/unix.Stat_t spells this Mtim on Linux,
// darwin AND the BSDs. Mtimespec is the STDLIB syscall.Stat_t spelling on
// darwin — a different type this copier never touches — and assuming it here is
// what broke the macOS build, since Linux CI cannot see the difference.
// TimespecToNsec rather than Sec/Nsec arithmetic, because Timespec's field
// widths also vary by platform.
func symlinkModTime(stat *unix.Stat_t) time.Time {
	return time.Unix(0, unix.TimespecToNsec(stat.Mtim))
}

func preserveSourceModTime(dirFD int, name string, sourceModTime time.Time, destinationPath, kind string) error {
	// TimeToTimespec, not NsecToTimespec(t.UnixNano()). UnixNano is only defined
	// for 1678–2262 and wraps silently outside it, while a filesystem can hold
	// timestamps well beyond that (ext4 with 256-byte inodes reaches 2446). The
	// wrapped spelling reproduced such a node CENTURIES off and still reported
	// success; this one converts seconds and nanoseconds separately and refuses
	// a value the platform cannot represent.
	stamp, err := unix.TimeToTimespec(sourceModTime)
	if err != nil {
		return fmt.Errorf(
			"cannot move worktree across filesystems: source modification time %s on %s %s is out of range for this platform: %w",
			sourceModTime.UTC().Format(time.RFC3339), kind, destinationPath, err,
		)
	}
	if err := unix.UtimesNanoAt(dirFD, name, []unix.Timespec{stamp, stamp}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf(
			"cannot move worktree across filesystems: failed to preserve the source modification time on destination %s %s: %w",
			kind, destinationPath, err,
		)
	}
	return nil
}

// preserveSourceMode restores a source node's permission bits on the descriptor
// that owns its copy.
//
// mkdirat(2) and openat(2) subtract the process umask from the mode they are
// handed, so creating a node "with the source's mode" is not the same as giving
// it the source's mode: under the common umask 0022 a 0777 directory lands 0755,
// and under 0077 an executable 0755 hook lands 0700. The same-device half of
// this move is a rename(2), which preserves modes exactly — so every bit the
// umask removes here is one move behaving two different ways depending on which
// filesystem the archive root happens to sit on, and an archive that no longer
// restores what it took (#2869).
//
// It chmods a descriptor rather than a name because this copier's entire premise
// is that a worktree process can replace any pathname mid-copy: fchmodat() would
// apply the source's mode to whatever node holds the name by the time it runs.
// Callers pass a descriptor only after the route walk or the creation check has
// confirmed which node it is.
//
// Setuid, setgid and sticky bits sit outside Perm() and are not carried over,
// which is the behavior this copier has always had. Symlinks are skipped
// entirely: Linux ignores their mode bits and offers no fchmod for them.
func preserveSourceMode(destinationFD int, sourceMode os.FileMode, destinationPath, kind string) error {
	if err := unix.Fchmod(destinationFD, uint32(sourceMode.Perm())); err != nil {
		return fmt.Errorf(
			"cannot move worktree across filesystems: failed to preserve mode %#o on destination %s %s: %w",
			sourceMode.Perm(), kind, destinationPath, err,
		)
	}
	return nil
}

// Hard-link reproduction's content check, split from worktree_copy_tree.go when
// that file reached the 1000-line limit (#1145).
package git

import (
	"bytes"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// sourceMatchesCopiedFile reports whether the source still holds exactly the
// bytes already copied for this inode, by COMPARING THEM.
//
// An earlier attempt used a (size, mtime, ctime) stamp and it was wrong in both
// directions at once, which is what retired it (#3046 review):
//
//   - Not sound. On a coarse-timestamp filesystem a same-size rewrite with mtime
//     restored inside one ctime tick leaves every field identical, so the guard
//     says "unchanged" and links the stale bytes it exists to prevent.
//   - Not precise either. link(2) moves ctime without touching content, so
//     ADDING a hard link — the live-worktree case the map exists to support —
//     read as a rewrite and split one inode into two destination inodes.
//
// A timestamp is not a content version. Comparing content is, and the cost lands
// only on a repeat sighting of an inode that is already known to be shared: at
// worst it reads what a fallback copy would have written anyway. Hard-linking is
// an optimisation, and one that can reproduce the wrong bytes is not worth its
// saving, so an unreadable or mismatched pair falls back to a fresh copy rather
// than linking on an assumption.
//
// Both sides are opened relative to a directory descriptor, never by rebuilt
// path, keeping the copier descriptor-anchored throughout.
// retained is a descriptor on the copy kept from when it was written, or nil. When
// present it is used INSTEAD of reopening, which is the whole of #3063: the copy's
// own mode can deny the copier that owns it, and a descriptor older than the mode
// is the only thing that still reads.
func sourceMatchesCopiedFile(source *os.File, name string, destinationRoot *os.File, copiedPath string, expected, expectedSource pathIdentity, retained *os.File) bool {
	current, err := openAtForCompare(source, name)
	if err != nil {
		return false
	}
	defer current.Close()

	// The SOURCE descriptor must be the inode the walk inspected, symmetrically
	// with the destination check below. statAt and this open are separate calls,
	// so a swap in between hands back a different file; if it holds the first
	// copy's bytes the comparison succeeds and links the old destination. The
	// regular-file copy path already makes exactly this check (#3049 review).
	currentIdentity, err := identityFromFile(current)
	if err != nil || !expectedSource.same(currentIdentity) {
		return false
	}

	// The destination is REOPENED AND READ, always. There is no cheaper substitute,
	// and two review rounds established why (#3049 review).
	//
	// A recorded digest of the source cannot stand in for it. It describes bytes
	// the source held at some moment, not what the destination holds NOW: a
	// same-UID process can chmod the destination, rewrite it in place, and restore
	// the mode, preserving device/inode/type — so identity still matches, the
	// unchanged source still matches the digest, and linkCopiedFile publishes the
	// modified bytes. Reading the destination is the only thing that attests it.
	//
	// The consequence is deliberate: when the destination cannot be reopened —
	// its own mode denies the copier read — this returns false and the caller
	// copies afresh. That LOSES the hard-link relationship for those files, which
	// is #3063 and is a fidelity cost, not a correctness one. Hard-linking is an
	// optimisation, and one that can publish bytes that were never at that path is
	// not worth its saving.
	copied := retained
	if copied == nil {
		reopened, err := openAtForCompare(destinationRoot, copiedPath)
		if err != nil {
			return false
		}
		defer reopened.Close()
		copied = reopened
	}
	copiedIdentity, err := identityFromFile(copied)
	if err != nil || !expected.same(copiedIdentity) {
		return false
	}

	currentInfo, err := current.Stat()
	if err != nil {
		return false
	}
	copiedInfo, err := copied.Stat()
	if err != nil {
		return false
	}
	if currentInfo.Size() != copiedInfo.Size() {
		return false
	}

	// Read the copy through a SectionReader, never sequentially off the descriptor
	// itself. A retained descriptor is a dup of the one the bytes were written
	// through, so its offset sits at EOF — and dup(2) SHARES that offset, so a
	// sequential read would both start at the end and move the writer's position.
	// A section reader uses pread and touches neither.
	copiedReader := io.NewSectionReader(copied, 0, copiedInfo.Size())
	const chunk = 64 * 1024
	currentChunk := make([]byte, chunk)
	copiedChunk := make([]byte, chunk)
	for {
		readCurrent, currentErr := io.ReadFull(current, currentChunk)
		readCopied, copiedErr := io.ReadFull(copiedReader, copiedChunk)
		if readCurrent != readCopied || !bytes.Equal(currentChunk[:readCurrent], copiedChunk[:readCopied]) {
			return false
		}
		if currentErr != nil || copiedErr != nil {
			// Both must have ended the same way; sizes already matched, so a
			// difference here means one was truncated underneath us.
			atEnd := func(err error) bool { return err == io.EOF || err == io.ErrUnexpectedEOF }
			return atEnd(currentErr) && atEnd(copiedErr)
		}
	}
}

func openAtForCompare(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(
		int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Hard-link reproduction's content check, split from worktree_copy_tree.go when
// that file reached the 1000-line limit (#1145).
package git

import (
	"bytes"
	"crypto/sha256"
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
func sourceMatchesCopiedFile(source *os.File, name string, destinationRoot *os.File, copiedPath string, digest []byte, expected pathIdentity) bool {
	current, err := openAtForCompare(source, name)
	if err != nil {
		return false
	}
	defer current.Close()

	// A recorded digest is used INSTEAD of reopening, because the reason it exists
	// is that reopening cannot work: the destination's own mode denies the copier
	// read, and owner bits take precedence, so the process that created the file
	// cannot open it (#3063).
	//
	// Identity is still checked, by fstatat rather than by reading — stat needs no
	// read permission. Without it a same-UID process could swap copiedPath for an
	// impostor holding the current bytes and restore the recorded inode before the
	// linkat, defeating linkCopiedFile's own post-link check (#3049 review).
	if digest != nil {
		copiedIdentity, err := identityAt(destinationRoot, copiedPath)
		if err != nil || !expected.same(copiedIdentity) {
			return false
		}
		currentDigest, err := digestOpenFile(current)
		if err != nil {
			return false
		}
		return bytes.Equal(currentDigest, digest)
	}

	copied, err := openAtForCompare(destinationRoot, copiedPath)
	if err != nil {
		return false
	}
	defer copied.Close()
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

	const chunk = 64 * 1024
	currentChunk := make([]byte, chunk)
	copiedChunk := make([]byte, chunk)
	for {
		readCurrent, currentErr := io.ReadFull(current, currentChunk)
		readCopied, copiedErr := io.ReadFull(copied, copiedChunk)
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

// digestOpenFile hashes an open file from its start, leaving the offset at EOF.
//
// SHA-256 rather than a cheaper hash because a collision here publishes the
// WRONG BYTES at a path — the exact outcome #3046 exists to prevent — and this
// runs only for the rare files whose mode locks the copier out, so its cost is
// not on the ordinary archive path.
func digestOpenFile(file *os.File) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return nil, err
	}
	return sum.Sum(nil), nil
}

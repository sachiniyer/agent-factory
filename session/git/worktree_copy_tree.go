package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// This file holds the recursive cross-device tree copier. Its counterpart,
// worktree_copy_cleanup.go, holds the identity bookkeeping and the removal
// paths that consume the manifest this copier builds.

// copyTree recursively copies the directory rooted at src to dest, preserving
// regular files (contents + permission bits), subdirectories, and symlinks
// (copied as links, never followed). Permission bits are applied explicitly
// rather than left to the mode passed at creation, because that mode is
// subtracted by the process umask and because a directory has to stay writable
// until its own contents are in place — see preserveSourceMode and
// workingDirectoryMode.
// Traversal stays anchored to open directory descriptors: a worktree process can
// replace any pathname after it is inspected, so every source open is
// nonblocking and no-follow, and every destination node is created exclusively.
// This is only reached on the cross-device fallback.
func copyTree(src, dest string) error {
	copied, err := copyTreeWithIdentities(src, dest)
	if copied != nil {
		copied.close()
	}
	return err
}

type copiedTreeIdentities struct {
	source                    *os.File
	sourceIdentity            pathIdentity
	destinationParent         *os.File
	destinationParentIdentity pathIdentity
	destination               *os.File
	destinationIdentity       pathIdentity
	root                      copiedDirectory
}

type copiedDirectory struct {
	entries []copiedEntry
}

type copiedEntry struct {
	name        string
	source      pathIdentity
	destination pathIdentity
	directory   *copiedDirectory
}

type copiedDirectoryRoute struct {
	parent    int
	entry     copiedEntry
	directory *copiedDirectory
	depth     int
}

const maxArchiveTreeDepth = 64

type pathIdentity struct {
	device   uint64
	inode    uint64
	fileType uint32
}

func (identity pathIdentity) same(other pathIdentity) bool {
	return identity == other
}

func (copied *copiedTreeIdentities) close() {
	_ = copied.destination.Close()
	_ = copied.destinationParent.Close()
	_ = copied.source.Close()
}

func (copied *copiedTreeIdentities) validateSource(path string) error {
	current, err := identityFromFile(copied.source)
	if err != nil {
		return err
	}
	if !copied.sourceIdentity.same(current) {
		return fmt.Errorf("source root identity changed at %s", path)
	}
	return validateCopiedTree(copied.source, copied.root, true, path)
}

func (copied *copiedTreeIdentities) validateDestination(path string) error {
	current, err := identityFromFile(copied.destination)
	if err != nil {
		return err
	}
	if !copied.destinationIdentity.same(current) {
		return fmt.Errorf("destination root identity changed at %s", path)
	}
	return validateCopiedTree(copied.destination, copied.root, false, path)
}

func openDirectoryRoute(
	root *os.File,
	rootPath string,
	components []copiedEntry,
	source bool,
) (*os.File, error) {
	role := "destination"
	if source {
		role = "source"
	}
	current, _, err := openDirectoryAt(root, ".", rootPath, role)
	if err != nil {
		return nil, err
	}
	currentPath := rootPath
	for _, component := range components {
		currentPath = filepath.Join(currentPath, component.name)
		next, _, err := openDirectoryAt(current, component.name, currentPath, role)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
		identity, err := identityFromFile(current)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		expected := component.destination
		if source {
			expected = component.source
		}
		if !expected.same(identity) {
			_ = current.Close()
			return nil, fmt.Errorf("%s directory identity changed at %s", role, currentPath)
		}
	}
	return current, nil
}

func directoryRoutePath(rootPath string, components []copiedEntry) string {
	path := rootPath
	for _, component := range components {
		path = filepath.Join(path, component.name)
	}
	return path
}

func copyTreeWithIdentities(src, dest string) (*copiedTreeIdentities, error) {
	source, sourceInfo, err := openDirectoryPath(src, "source")
	if err != nil {
		return nil, err
	}
	if err := copyTreeBeforeSourceOpen(src); err != nil {
		_ = source.Close()
		return nil, err
	}
	sourceIdentity, err := identityFromFile(source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}

	destParentPath := filepath.Dir(dest)
	destParent, _, err := openDirectoryPathFollowingLinks(destParentPath, "destination parent")
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	destinationParentIdentity, err := identityFromFile(destParent)
	if err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, err
	}
	destName := filepath.Base(dest)
	if err := unix.Mkdirat(int(destParent.Fd()), destName, workingDirectoryMode(sourceInfo.Mode())); err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", dest, err)
	}
	// Identify the staging root before anything else can fail: cleanup below
	// unlinks it, and unlinkat() takes a name rather than an inode.
	createdIdentity, err := identityAt(destParent, destName)
	if err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, fmt.Errorf(
			"cannot move worktree across filesystems: failed to identify destination directory %s after creating it (leaving it in place): %w",
			dest, err,
		)
	}
	destination, _, err := openDirectoryAt(destParent, destName, dest, "destination")
	if err != nil {
		cleanupErr := removeCreatedDirectory(destParent, destParentPath, destName, createdIdentity)
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	destinationIdentity, err := identityFromFile(destination)
	if err != nil {
		cleanupErr := removeCreatedDirectory(destParent, destParentPath, destName, createdIdentity)
		_ = destination.Close()
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	copied := &copiedTreeIdentities{
		source:                    source,
		sourceIdentity:            sourceIdentity,
		destinationParent:         destParent,
		destinationParentIdentity: destinationParentIdentity,
		destination:               destination,
		destinationIdentity:       destinationIdentity,
	}

	copied.root, err = copyDirectoryContents(source, destination, src, dest)
	if err != nil {
		partialManifest := destinationCleanupManifest(copied.root)
		cleanupErr := removeOpenedDirectory(destParent, destName, dest, destination, &partialManifest)
		copied.close()
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("failed to clean partial destination tree %s: %w", dest, cleanupErr))
		}
		return nil, err
	}
	return copied, nil
}

func copyDirectoryContents(source, destination *os.File, sourcePath, destinationPath string) (copiedDirectory, error) {
	root := copiedDirectory{}
	// Source inode -> where its bytes first landed AND what inode they landed
	// on. Scoped to one copy, so it can never name a path from another tree.
	links := map[pathIdentity]copiedFileLink{}
	// Same scoping as links above: whether this DESTINATION holds extended
	// attributes is learned once per copy and never leaks into another one.
	xattrs := &xattrDestination{}
	routes := []copiedDirectoryRoute{{parent: -1, directory: &root}}
	for index := 0; index < len(routes); index++ {
		job := routes[index]
		components := copiedDirectoryRouteComponents(routes, index)
		sourceDirectory, err := openDirectoryRoute(source, sourcePath, components, true)
		if err != nil {
			return root, err
		}
		destinationDirectory, err := openDirectoryRoute(destination, destinationPath, components, false)
		if err != nil {
			_ = sourceDirectory.Close()
			return root, err
		}
		err = copyDirectoryLevel(
			sourceDirectory,
			destinationDirectory,
			directoryRoutePath(sourcePath, components),
			directoryRoutePath(destinationPath, components),
			job.directory,
			destination,
			relativeRoutePath(components),
			links,
			xattrs,
		)
		if err == nil {
			// The level is complete, and nothing is ever written directly into
			// this directory again — descendants are filled through their own
			// descriptors, which need only the search bit here. So this is the
			// first moment the real mode can be applied, and the last moment it
			// is free: a read-only source directory takes its mode now rather
			// than blocking its own contents (#2872).
			err = applyCopiedDirectoryMode(
				sourceDirectory, destinationDirectory, directoryRoutePath(destinationPath, components), xattrs,
			)
		}
		_ = destinationDirectory.Close()
		_ = sourceDirectory.Close()
		if err != nil {
			return root, err
		}
		if job.depth >= maxArchiveTreeDepth && copiedDirectoryHasChildren(job.directory) {
			return root, fmt.Errorf(
				"cannot move worktree across filesystems: maximum supported depth of %d exceeded at %s",
				maxArchiveTreeDepth, directoryRoutePath(sourcePath, components),
			)
		}
		routes = appendCopiedDirectoryChildren(routes, index)
	}
	return root, nil
}

func copyDirectoryLevel(
	source, destination *os.File,
	sourcePath, destinationPath string,
	directory *copiedDirectory,
	destinationRoot *os.File,
	relativeDirectory string,
	links map[pathIdentity]copiedFileLink,
	xattrs *xattrDestination,
) error {
	names, err := source.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to enumerate source directory %s: %w", sourcePath, err)
	}
	sort.Strings(names)
	directory.entries = make([]copiedEntry, 0, len(names))
	for _, name := range names {
		childSourcePath := filepath.Join(sourcePath, name)
		childDestinationPath := filepath.Join(destinationPath, name)
		if err := copyTreeBeforeSourceOpen(childSourcePath); err != nil {
			return err
		}
		stat, err := statAt(source, name)
		if err != nil {
			return fmt.Errorf("cannot move worktree across filesystems: failed to inspect source entry %s: %w", childSourcePath, err)
		}
		inspected := identityFromStat(stat)
		// The inspect/open boundary: from here the entry's type is a stale fact
		// about a pathname, and a worktree process can replace what that name
		// resolves to. Every branch below re-establishes the type from the object
		// it actually opened; this seam lets a test substitute a node in exactly
		// this window (#2708).
		if err := copyTreeAfterSourceInspect(childSourcePath); err != nil {
			return err
		}

		var entry copiedEntry
		switch inspected.fileType {
		case unix.S_IFDIR:
			entry, err = copyDirectoryEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFLNK:
			entry, err = copySymlinkEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFREG:
			// Every successfully copied regular inode is recorded, not just the
			// ones observed with nlink > 1. A live worktree process can hard-link
			// an already-copied file AFTER it was seen with nlink == 1, and the
			// later sighting would then miss the map and be copied as a separate
			// inode — silently publishing two unrelated files while source
			// validation passed, because pathIdentity excludes the link count.
			if first, seen := links[inspected]; seen {
				entry, err = linkCopiedFile(destination, destinationRoot, first, name, childDestinationPath, inspected)
			} else {
				entry, err = copyRegularFileAtWithIdentity(source, destination, name, childSourcePath, childDestinationPath, &inspected, xattrs)
				if err == nil {
					links[inspected] = copiedFileLink{
						path:     filepath.Join(relativeDirectory, name),
						identity: entry.destination,
					}
				}
			}
		default:
			err = unsupportedSourceTypeError(childSourcePath, uint32(stat.Mode))
		}
		// A helper names its entry as soon as it has created the destination
		// node and learned its identity, so record it even when the entry
		// later fails. Cleanup can only remove what the manifest describes;
		// dropping a node this process created makes it read as an unexpected
		// entry and strands the whole partial tree.
		if entry.name != "" {
			directory.entries = append(directory.entries, entry)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// relativeRoutePath renders a BFS route as a path relative to the copy root,
// which is what linkCopiedFile needs: the root descriptor outlives every level,
// so a link can be made against it without reopening the route the first copy
// landed in.
func relativeRoutePath(components []copiedEntry) string {
	parts := make([]string, 0, len(components))
	for _, component := range components {
		parts = append(parts, component.name)
	}
	return filepath.Join(parts...)
}

// copiedFileLink is where a source inode's bytes first landed: the path to link
// against, and the destination inode that path resolved to at the time. Both are
// needed — the path to make the link, the identity to prove the link landed on
// the right inode.
type copiedFileLink struct {
	path     string
	identity pathIdentity
}

// linkCopiedFile reproduces a hard link instead of copying the bytes a second
// time.
//
// Each directory entry used to be copied independently, so a linked pair arrived
// as two unrelated files (#2919). That costs disk twice over — the archive root
// is shared by every session — but the sharper problem is semantic: writing
// through one path stopped showing through the other, so a restored worktree
// behaved differently from the one that was archived.
//
// linkat() resolves a PATHNAME, and the staging tree, while unguessably named,
// is still reachable by a same-UID process. Such a process can rename the first
// copied path aside, drop an unrelated file or symlink there, let this call link
// THAT, and restore the original path afterwards. Merely recording whatever
// inode results would make the injected one the manifest's expected identity, so
// both later validations would agree with each other and a corrupted tree would
// publish. The identity captured when the bytes were first written is therefore
// compared, not just stored: a link that did not land on that exact inode is
// refused.
//
// flags is 0. The entry is already known to be a regular file, and
// AT_SYMLINK_FOLLOW would be the only way to end up linking something else.
func linkCopiedFile(
	destination, destinationRoot *os.File,
	first copiedFileLink,
	name, destinationPath string,
	sourceIdentity pathIdentity,
) (copiedEntry, error) {
	if err := unix.Linkat(int(destinationRoot.Fd()), first.path, int(destination.Fd()), name, 0); err != nil {
		return copiedEntry{}, fmt.Errorf(
			"cannot move worktree across filesystems: failed to reproduce the hard link at %s: %w", destinationPath, err)
	}
	// Named and identified before anything else can fail, exactly like every
	// other node this copier creates — cleanup can only remove what the manifest
	// describes, so the OBSERVED identity is recorded even on the refusal below.
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{name: name, source: sourceIdentity}, fmt.Errorf(
			"cannot move worktree across filesystems: failed to identify hard link %s after creating it: %w",
			destinationPath, err)
	}
	created := copiedEntry{name: name, source: sourceIdentity, destination: destinationIdentity}
	if !first.identity.same(destinationIdentity) {
		return created, fmt.Errorf(
			"cannot move worktree across filesystems: hard link %s resolved to a different inode than the file it was linked from",
			destinationPath)
	}
	return created, nil
}

func copyDirectoryEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	sourceChild, sourceInfo, err := openDirectoryAt(source, name, sourcePath, "source")
	if err != nil {
		return copiedEntry{}, err
	}
	defer sourceChild.Close()
	sourceIdentity, err := identityFromFile(sourceChild)
	if err != nil {
		return copiedEntry{}, err
	}
	if !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source directory %s changed before it was opened", sourcePath)
	}
	if err := unix.Mkdirat(int(destination.Fd()), name, workingDirectoryMode(sourceInfo.Mode())); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", destinationPath, err)
	}
	// Identify the node right after creating it: every later step can fail, and
	// the caller can only record what it can name and identify.
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to identify destination directory %s after creating it: %w", destinationPath, err)
	}
	created := copiedEntry{
		name:        name,
		source:      sourceIdentity,
		destination: destinationIdentity,
		directory:   &copiedDirectory{},
	}
	if err := copyTreeAfterDestCreate(destinationPath); err != nil {
		return created, err
	}
	destinationChild, _, err := openDirectoryAt(destination, name, destinationPath, "destination")
	if err != nil {
		return created, err
	}
	defer destinationChild.Close()
	openedIdentity, err := identityFromFile(destinationChild)
	if err != nil {
		return created, err
	}
	if !destinationIdentity.same(openedIdentity) {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination directory %s changed after it was created", destinationPath)
	}
	return created, nil
}

func copySymlinkEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	link, err := readLinkAt(source, name, sourcePath)
	if err != nil {
		return copiedEntry{}, err
	}
	// One lstat serves both the race re-check and the timestamp below: statAt is
	// Fstatat with AT_SYMLINK_NOFOLLOW, so it reads the LINK rather than what it
	// points at, and it is the same call identityAt was already making here.
	sourceStat, err := statAt(source, name)
	if err != nil || !inspected.same(identityFromStat(sourceStat)) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source symlink %s changed while it was copied", sourcePath)
	}
	if err := unix.Symlinkat(link, int(destination.Fd()), name); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination symlink %s exclusively: %w", destinationPath, err)
	}
	// Identify the node right after creating it: every later step can fail, and
	// the caller can only record what it can name and identify.
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to identify destination symlink %s after creating it: %w", destinationPath, err)
	}
	created := copiedEntry{name: name, source: inspected, destination: destinationIdentity}
	if err := copyTreeAfterSymlinkCreate(destinationPath); err != nil {
		return created, err
	}
	destinationLink, err := readLinkAt(destination, name, destinationPath)
	if err != nil {
		return created, err
	}
	confirmedIdentity, err := identityAt(destination, name)
	if err != nil || !destinationIdentity.same(confirmedIdentity) || destinationLink != link {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination symlink %s changed while it was copied", destinationPath)
	}
	// The link's OWN mtime. AT_SYMLINK_NOFOLLOW means this addresses the link
	// rather than following it to its target.
	//
	// The confirmation above does NOT cover this call, which is the correction
	// the review of the duplicate raised: the stamp resolves `name` one more
	// time, so a racer that swaps the entry in between lands our timestamp on an
	// inode we never created. AT_SYMLINK_NOFOLLOW refuses to FOLLOW a symlink; it
	// does not refuse to stamp a hard link, so the reachable damage is a foreign
	// file's mtime — outside the staging tree included.
	//
	// It cannot be prevented here. Holding a symlink open needs O_PATH|O_NOFOLLOW
	// on Linux or O_SYMLINK on darwin, and stamping through that descriptor needs
	// AT_EMPTY_PATH, which is Linux-only — the portability wall this whole item
	// keeps running into. So it is DETECTED instead, and detection means the
	// archive refuses rather than commits, which is what every other replacement
	// check in this function already does.
	if err := copyTreeBeforeSymlinkStamp(destinationPath); err != nil {
		return created, err
	}
	if err := preserveSourceModTime(
		int(destination.Fd()), name, symlinkModTime(sourceStat), destinationPath, "symlink",
	); err != nil {
		return created, err
	}
	stampedIdentity, err := identityAt(destination, name)
	if err != nil || !destinationIdentity.same(stampedIdentity) {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination symlink %s changed while its timestamp was applied", destinationPath)
	}
	return created, nil
}

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

func copiedDirectoryHasChildren(directory *copiedDirectory) bool {
	for _, entry := range directory.entries {
		if entry.directory != nil {
			return true
		}
	}
	return false
}

// copyFile is the path-based test entrypoint for the regular-file copier. The
// production tree walker calls copyRegularFileAt with already-open parents.
func copyFile(src, dst string) error {
	sourceParent, _, err := openDirectoryPath(filepath.Dir(src), "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	destinationParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(dst), "destination parent")
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	return copyRegularFileAt(sourceParent, destinationParent, filepath.Base(src), src, dst)
}

func copyRegularFileAt(source, destination *os.File, name, sourcePath, destinationPath string) error {
	_, err := copyRegularFileAtWithIdentity(source, destination, name, sourcePath, destinationPath, nil, &xattrDestination{})
	return err
}

func copyRegularFileAtWithIdentity(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected *pathIdentity,
	support *xattrDestination,
) (copiedEntry, error) {
	fd, err := unix.Openat(
		int(source.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to open source file %s without following links: %w", sourcePath, err)
	}
	in := os.NewFile(uintptr(fd), sourcePath)
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return copiedEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return copiedEntry{}, unsupportedSourceTypeError(sourcePath, uint32(info.Mode()))
	}
	sourceIdentity, err := identityFromFile(in)
	if err != nil {
		return copiedEntry{}, err
	}
	if inspected != nil && !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source file %s changed before it was opened", sourcePath)
	}
	outFD, err := unix.Openat(
		int(destination.Fd()), filepath.Base(destinationPath),
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		workingFileMode(info.Mode()),
	)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination file %s exclusively: %w", destinationPath, err)
	}
	out := os.NewFile(uintptr(outFD), destinationPath)
	// Identify the node right after creating it: the copy and the close can both
	// fail, and the caller can only record what it can name and identify.
	destinationIdentity, err := identityFromFile(out)
	if err != nil {
		_ = out.Close()
		return copiedEntry{}, err
	}
	created := copiedEntry{name: name, source: sourceIdentity, destination: destinationIdentity}
	if err := copyTreeAfterDestCreate(destinationPath); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := copyContentsPreservingHoles(out, in); err != nil {
		_ = out.Close()
		return created, err
	}
	// The mode comes after the contents AND after every step that needs the inode
	// writable. workingFileMode created this file owner-writable for exactly that
	// reason; preserveSourceMode narrows it to the source's mode here, once there
	// is nothing half-written left to expose. The already-open descriptor keeps
	// its write access regardless of the new mode.
	//
	// Non-ACL attributes go on BEFORE the mode and the prune runs while the inode
	// is still writable, because removing one needs write permission just as
	// setting one does. ACLs go on AFTER it: the access ACL and the mode bits are
	// one state seen two ways, so chmod would silently rewrite an ACL written
	// earlier. applyCopiedDirectoryMode does the same four steps in the same order.
	if err := copyNonACLXattrs(support, int(in.Fd()), outFD, destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	pruneDestinationXattrs(int(in.Fd()), outFD, destinationPath, "file")
	if err := preserveSourceMode(outFD, info.Mode(), destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := copyACLXattrs(support, int(in.Fd()), outFD, destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	// After the contents for the same reason the mode is: writing bytes bumps
	// mtime, so this has to be the last thing that touches the file.
	if err := preserveSourceModTime(
		int(destination.Fd()), filepath.Base(destinationPath), info.ModTime(), destinationPath, "file",
	); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := out.Close(); err != nil {
		return created, err
	}
	return created, nil
}

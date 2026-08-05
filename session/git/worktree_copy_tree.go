package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// This file holds the recursive cross-device tree copier. Its counterpart,
// worktree_copy_cleanup.go, holds the identity bookkeeping and the removal
// paths that consume the manifest this copier builds.

// copyTree recursively copies the directory rooted at src to dest, preserving
// regular files (contents + permission bits), subdirectories, and symlinks
// (copied as links, never followed). Permission bits are restored explicitly
// after each node is created rather than left to the mode passed at creation,
// because that mode is subtracted by the process umask — see preserveSourceMode.
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
	if err := unix.Mkdirat(int(destParent.Fd()), destName, uint32(sourceInfo.Mode().Perm())); err != nil {
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
	if err == nil {
		// The whole copy is anchored to this descriptor, so it is also where the
		// root's mode belongs. Every child mode is restored the same way, at the
		// point its own descriptor is confirmed.
		err = preserveSourceMode(int(destination.Fd()), sourceInfo.Mode(), dest, "directory")
	}
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
		)
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
			entry, err = copyRegularFileAtWithIdentity(source, destination, name, childSourcePath, childDestinationPath, &inspected)
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
	if err := unix.Mkdirat(int(destination.Fd()), name, uint32(sourceInfo.Mode().Perm())); err != nil {
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
	// Only now: the descriptor has just been confirmed to be the node this
	// process created, which is what makes chmodding it safe.
	if err := preserveSourceMode(int(destinationChild.Fd()), sourceInfo.Mode(), destinationPath, "directory"); err != nil {
		return created, err
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
	current, err := identityAt(source, name)
	if err != nil || !inspected.same(current) {
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
	return created, nil
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
// Callers pass a descriptor only after confirming it is the node they created.
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
	_, err := copyRegularFileAtWithIdentity(source, destination, name, sourcePath, destinationPath, nil)
	return err
}

func copyRegularFileAtWithIdentity(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected *pathIdentity,
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
		uint32(info.Mode().Perm()),
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
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return created, err
	}
	// After the contents, not before: openat() created this file no wider than
	// the source (a umask only clears bits), so widening it back is the last
	// step, once there is nothing half-written left to expose. The already-open
	// descriptor keeps its write access regardless of the new mode.
	if err := preserveSourceMode(outFD, info.Mode(), destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := out.Close(); err != nil {
		return created, err
	}
	return created, nil
}

package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

func statAt(parent *os.File, name string) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return &stat, nil
}

func identityAt(parent *os.File, name string) (pathIdentity, error) {
	stat, err := statAt(parent, name)
	if err != nil {
		return pathIdentity{}, err
	}
	return identityFromStat(stat), nil
}

func identityFromFile(file *os.File) (pathIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return pathIdentity{}, err
	}
	return identityFromStat(&stat), nil
}

func identityFromStat(stat *unix.Stat_t) pathIdentity {
	return pathIdentity{
		device:   uint64(stat.Dev),
		inode:    uint64(stat.Ino),
		fileType: uint32(stat.Mode) & unix.S_IFMT,
	}
}

func validateDirectoryPathIdentity(path, role string, expected pathIdentity) error {
	current, _, err := openDirectoryPathFollowingLinks(path, role+" parent")
	if err != nil {
		return fmt.Errorf("%s parent path changed at %s: %w", role, path, err)
	}
	defer current.Close()
	identity, err := identityFromFile(current)
	if err != nil {
		return fmt.Errorf("could not verify %s parent path at %s: %w", role, path, err)
	}
	if !expected.same(identity) {
		return fmt.Errorf("%s parent path changed at %s", role, path)
	}
	return nil
}

func validateNamedPathIdentity(
	parentPath, name, role string,
	expectedParent, expectedEntry pathIdentity,
) error {
	currentParent, _, err := openDirectoryPathFollowingLinks(parentPath, role+" parent")
	if err != nil {
		return fmt.Errorf("%s parent path changed at %s: %w", role, parentPath, err)
	}
	defer currentParent.Close()
	parentIdentity, err := identityFromFile(currentParent)
	if err != nil {
		return fmt.Errorf("could not verify %s parent path at %s: %w", role, parentPath, err)
	}
	if !expectedParent.same(parentIdentity) {
		return fmt.Errorf("%s parent path changed at %s", role, parentPath)
	}
	entryIdentity, err := identityAt(currentParent, name)
	if err != nil {
		return fmt.Errorf("%s path changed at %s: %w", role, filepath.Join(parentPath, name), err)
	}
	if !expectedEntry.same(entryIdentity) {
		return fmt.Errorf("%s path changed at %s", role, filepath.Join(parentPath, name))
	}
	return nil
}

func destinationCleanupManifest(root copiedDirectory) copiedDirectory {
	manifest := copiedDirectory{}
	type cloneJob struct {
		source      *copiedDirectory
		destination *copiedDirectory
	}
	queue := []cloneJob{{source: &root, destination: &manifest}}
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		job.destination.entries = make([]copiedEntry, 0, len(job.source.entries))
		for _, entry := range job.source.entries {
			clone := copiedEntry{name: entry.name, source: entry.destination}
			if entry.directory != nil {
				clone.directory = &copiedDirectory{}
			}
			job.destination.entries = append(job.destination.entries, clone)
			if entry.directory != nil {
				clonedEntry := &job.destination.entries[len(job.destination.entries)-1]
				queue = append(queue, cloneJob{source: entry.directory, destination: clonedEntry.directory})
			}
		}
	}
	return manifest
}

func directoryNames(directory *os.File, path string) ([]string, error) {
	reader, _, err := openDirectoryAt(directory, ".", path, "tree")
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	names, err := reader.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("cannot move worktree across filesystems: failed to enumerate directory %s: %w", path, err)
	}
	sort.Strings(names)
	return names, nil
}

// removeCreatedDirectory removes a directory this process just created, using
// the identity captured at creation. unlinkat() resolves a name rather than an
// inode, so a bare rmdir here would delete whatever a same-UID racer left at
// that name — an empty directory it created is still not ours to remove. The
// shared claim-verify-unlink path fails closed instead.
func removeCreatedDirectory(parent *os.File, parentPath, name string, created pathIdentity) error {
	return removeCopiedEntry(parent, parentPath, copiedEntry{
		name:      name,
		source:    created,
		directory: &copiedDirectory{},
	}, true)
}

func validateCopiedTree(root *os.File, expected copiedDirectory, source bool, path string) error {
	rootDirectory := expected
	routes := copiedDirectoryRoutes(&rootDirectory)
	for index, job := range routes {
		components := copiedDirectoryRouteComponents(routes, index)
		directory, err := openDirectoryRoute(root, path, components, source)
		if err != nil {
			return err
		}
		err = validateCopiedDirectoryLevel(
			directory,
			*job.directory,
			source,
			directoryRoutePath(path, components),
		)
		_ = directory.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func validateCopiedDirectoryLevel(
	directory *os.File,
	expected copiedDirectory,
	source bool,
	path string,
) error {
	names, err := directoryNames(directory, path)
	if err != nil {
		return err
	}
	expectedByName := make(map[string]copiedEntry, len(expected.entries))
	for _, entry := range expected.entries {
		expectedByName[entry.name] = entry
	}
	if len(names) != len(expectedByName) {
		return fmt.Errorf("tree entry set changed at %s", path)
	}
	for _, name := range names {
		entry, ok := expectedByName[name]
		if !ok {
			return fmt.Errorf("unexpected tree entry %s", filepath.Join(path, name))
		}
		current, err := identityAt(directory, name)
		if err != nil {
			return err
		}
		expectedIdentity := entry.destination
		role := "destination"
		if source {
			expectedIdentity = entry.source
			role = "source"
		}
		if !expectedIdentity.same(current) {
			return fmt.Errorf("%s tree entry identity changed at %s", role, filepath.Join(path, name))
		}
	}
	return nil
}

func validatePublishedDestination(dest string, copied *copiedTreeIdentities) error {
	currentParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(dest), "destination parent")
	if err != nil {
		return fmt.Errorf("destination directory changed during commit at %s: %w", dest, err)
	}
	defer currentParent.Close()
	currentParentIdentity, err := identityFromFile(currentParent)
	if err != nil {
		return err
	}
	if !copied.destinationParentIdentity.same(currentParentIdentity) {
		return fmt.Errorf("destination parent changed during commit at %s", filepath.Dir(dest))
	}
	currentDestination, err := identityAt(currentParent, filepath.Base(dest))
	if err != nil {
		return fmt.Errorf("destination directory changed during commit at %s: %w", dest, err)
	}
	if !copied.destinationIdentity.same(currentDestination) {
		return fmt.Errorf("destination directory changed during commit at %s", dest)
	}
	retainedDestination, err := identityAt(copied.destinationParent, filepath.Base(dest))
	if err != nil || !copied.destinationIdentity.same(retainedDestination) {
		return fmt.Errorf("destination directory changed during commit at %s", dest)
	}
	if err := copied.validateDestination(dest); err != nil {
		return fmt.Errorf("destination tree changed during commit at %s: %w", dest, err)
	}
	retainedDestination, err = identityAt(copied.destinationParent, filepath.Base(dest))
	if err != nil || !copied.destinationIdentity.same(retainedDestination) {
		return fmt.Errorf("destination directory changed during commit at %s", dest)
	}
	return nil
}

type unverifiedCleanupPathError struct {
	err error
}

func (err *unverifiedCleanupPathError) Error() string { return err.err.Error() }
func (err *unverifiedCleanupPathError) Unwrap() error { return err.err }

func unverifiedCleanupError(format string, args ...any) error {
	return &unverifiedCleanupPathError{err: fmt.Errorf(format, args...)}
}

func removeOpenedDirectory(
	parent *os.File,
	name, path string,
	directory *os.File,
	expected *copiedDirectory,
) error {
	directoryIdentity, err := identityFromFile(directory)
	if err != nil {
		return err
	}
	current, err := identityAt(parent, name)
	if err != nil || !directoryIdentity.same(current) {
		return unverifiedCleanupError(
			"refusing to remove directory %s because its secured name no longer identifies the opened tree",
			path,
		)
	}
	manifest := expected
	protectUnexpected := expected != nil
	if manifest == nil {
		snapshot, snapshotErr := snapshotCopiedTree(directory, path)
		if snapshotErr != nil {
			return recheckCleanupRoot(parent, name, path, directoryIdentity, snapshotErr)
		}
		manifest = &snapshot
	} else if err := validateCopiedTree(directory, *manifest, true, path); err != nil {
		return unverifiedCleanupError("refusing to remove changed source tree %s: %v", path, err)
	}
	if err := removeCopiedTree(directory, path, *manifest, protectUnexpected); err != nil {
		return recheckCleanupRoot(parent, name, path, directoryIdentity, err)
	}
	current, err = identityAt(parent, name)
	if err != nil || !directoryIdentity.same(current) {
		return unverifiedCleanupError(
			"refusing to unlink directory %s because its secured name changed during cleanup",
			path,
		)
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return recheckCleanupRoot(
			parent,
			name,
			path,
			directoryIdentity,
			fmt.Errorf("failed to remove secured directory %s: %w", path, err),
		)
	}
	return nil
}

func recheckCleanupRoot(
	parent *os.File,
	name, path string,
	expected pathIdentity,
	cause error,
) error {
	current, err := identityAt(parent, name)
	if err == nil && expected.same(current) {
		return cause
	}
	return &unverifiedCleanupPathError{err: errors.Join(
		cause,
		fmt.Errorf("secured cleanup name %s no longer identifies the retained source", path),
	)}
}

func snapshotCopiedTree(root *os.File, path string) (copiedDirectory, error) {
	manifest := copiedDirectory{}
	routes := []copiedDirectoryRoute{{parent: -1, directory: &manifest}}
	for index := 0; index < len(routes); index++ {
		job := routes[index]
		components := copiedDirectoryRouteComponents(routes, index)
		directory, err := openDirectoryRoute(root, path, components, true)
		if err != nil {
			return copiedDirectory{}, err
		}
		names, err := directoryNames(directory, directoryRoutePath(path, components))
		if err != nil {
			_ = directory.Close()
			return copiedDirectory{}, err
		}
		job.directory.entries = make([]copiedEntry, 0, len(names))
		for _, name := range names {
			identity, err := identityAt(directory, name)
			if err != nil {
				_ = directory.Close()
				return copiedDirectory{}, err
			}
			entry := copiedEntry{name: name, source: identity}
			if identity.fileType == unix.S_IFDIR {
				child := copiedDirectory{}
				entry.directory = &child
			}
			job.directory.entries = append(job.directory.entries, entry)
		}
		_ = directory.Close()
		routes = appendCopiedDirectoryChildren(routes, index)
	}
	return manifest, nil
}

func removeCopiedTree(root *os.File, path string, manifest copiedDirectory, protectUnexpected bool) error {
	rootManifest := manifest
	routes := copiedDirectoryRoutes(&rootManifest)
	for index := len(routes) - 1; index >= 0; index-- {
		route := routes[index]
		components := copiedDirectoryRouteComponents(routes, index)
		directory, err := openDirectoryRoute(root, path, components, true)
		if err != nil {
			return changedCleanupError(protectUnexpected, "failed to reopen copied source tree: %v", err)
		}
		directoryPath := directoryRoutePath(path, components)
		for _, entry := range route.directory.entries {
			if entry.directory != nil {
				continue
			}
			if err := removeCopiedEntry(directory, directoryPath, entry, protectUnexpected); err != nil {
				_ = directory.Close()
				return err
			}
		}
		remaining, err := directoryNames(directory, directoryPath)
		_ = directory.Close()
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return changedCleanupError(
				protectUnexpected,
				"refusing to remove directory %s because unexpected entries remain",
				directoryPath,
			)
		}
		if route.parent < 0 {
			continue
		}
		parentComponents := copiedDirectoryRouteComponents(routes, route.parent)
		parent, err := openDirectoryRoute(root, path, parentComponents, true)
		if err != nil {
			return changedCleanupError(protectUnexpected, "failed to reopen copied source parent: %v", err)
		}
		err = removeCopiedEntry(parent, directoryRoutePath(path, parentComponents), route.entry, protectUnexpected)
		_ = parent.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copiedDirectoryRoutes(root *copiedDirectory) []copiedDirectoryRoute {
	routes := []copiedDirectoryRoute{{parent: -1, directory: root}}
	for index := 0; index < len(routes); index++ {
		routes = appendCopiedDirectoryChildren(routes, index)
	}
	return routes
}

func appendCopiedDirectoryChildren(routes []copiedDirectoryRoute, parent int) []copiedDirectoryRoute {
	parentRoute := routes[parent]
	for _, entry := range parentRoute.directory.entries {
		if entry.directory == nil {
			continue
		}
		routes = append(routes, copiedDirectoryRoute{
			parent: parent, entry: entry, directory: entry.directory, depth: parentRoute.depth + 1,
		})
	}
	return routes
}

func copiedDirectoryRouteComponents(routes []copiedDirectoryRoute, index int) []copiedEntry {
	components := make([]copiedEntry, routes[index].depth)
	for position := len(components) - 1; position >= 0; position-- {
		components[position] = routes[index].entry
		index = routes[index].parent
	}
	return components
}

func retainedRouteEntries(routes []copiedDirectoryRoute) int {
	total := 0
	for _, route := range routes {
		if route.parent >= 0 {
			total++
		}
	}
	return total
}

func removeCopiedEntry(directory *os.File, path string, entry copiedEntry, protectUnexpected bool) error {
	if err := removeTreeBeforeEntryClaim(directory, path); err != nil {
		return err
	}
	current, err := identityAt(directory, entry.name)
	if err != nil || !entry.source.same(current) {
		return changedCleanupError(
			protectUnexpected,
			"refusing to remove changed source entry %s",
			filepath.Join(path, entry.name),
		)
	}
	claimedName, err := privateMoveName("delete")
	if err != nil {
		return err
	}
	if err := renameAtNoReplace(int(directory.Fd()), entry.name, int(directory.Fd()), claimedName); err != nil {
		return fmt.Errorf("failed to secure entry %s for removal: %w", filepath.Join(path, entry.name), err)
	}
	claimed, err := identityAt(directory, claimedName)
	if err != nil || !current.same(claimed) {
		restoreErr := renameAtNoReplace(int(directory.Fd()), claimedName, int(directory.Fd()), entry.name)
		return errors.Join(
			changedCleanupError(protectUnexpected, "entry %s changed while it was secured for removal", filepath.Join(path, entry.name)),
			restoreErr,
		)
	}
	flags := 0
	if entry.directory != nil {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(int(directory.Fd()), claimedName, flags); err != nil {
		return fmt.Errorf("failed to remove secured entry %s: %w", filepath.Join(path, claimedName), err)
	}
	return nil
}

func changedCleanupError(protect bool, format string, args ...any) error {
	if protect {
		return unverifiedCleanupError(format, args...)
	}
	return fmt.Errorf(format, args...)
}

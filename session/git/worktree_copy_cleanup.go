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

func removeEmptyDirectoryAt(parent *os.File, name, path string) error {
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("failed to clean empty private directory %s: %w", path, err)
	}
	return nil
}

func validateCopiedTree(root *os.File, expected copiedDirectory, source bool, path string) error {
	rootDirectory := expected
	queue := []copiedDirectoryRoute{{directory: &rootDirectory}}
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		directory, err := openDirectoryRoute(root, path, job.components, source)
		if err != nil {
			return err
		}
		err = validateCopiedDirectoryLevel(
			directory,
			*job.directory,
			source,
			directoryRoutePath(path, job.components),
		)
		_ = directory.Close()
		if err != nil {
			return err
		}
		for _, entry := range job.directory.entries {
			if entry.directory == nil {
				continue
			}
			components := append([]copiedEntry(nil), job.components...)
			components = append(components, entry)
			queue = append(queue, copiedDirectoryRoute{components: components, directory: entry.directory})
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
	queue := []copiedDirectoryRoute{{directory: &manifest}}
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		directory, err := openDirectoryRoute(root, path, job.components, true)
		if err != nil {
			return copiedDirectory{}, err
		}
		names, err := directoryNames(directory, directoryRoutePath(path, job.components))
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
			if entry.directory != nil {
				components := append([]copiedEntry(nil), job.components...)
				components = append(components, entry)
				queue = append(queue, copiedDirectoryRoute{components: components, directory: entry.directory})
			}
		}
		_ = directory.Close()
	}
	return manifest, nil
}

func removeCopiedTree(root *os.File, path string, manifest copiedDirectory, protectUnexpected bool) error {
	rootManifest := manifest
	routes := []copiedDirectoryRoute{{directory: &rootManifest}}
	for index := 0; index < len(routes); index++ {
		job := routes[index]
		for _, entry := range job.directory.entries {
			if entry.directory == nil {
				continue
			}
			components := append([]copiedEntry(nil), job.components...)
			components = append(components, entry)
			routes = append(routes, copiedDirectoryRoute{components: components, directory: entry.directory})
		}
	}
	sort.SliceStable(routes, func(left, right int) bool {
		return len(routes[left].components) > len(routes[right].components)
	})
	for _, route := range routes {
		directory, err := openDirectoryRoute(root, path, route.components, true)
		if err != nil {
			return changedCleanupError(protectUnexpected, "failed to reopen copied source tree: %v", err)
		}
		directoryPath := directoryRoutePath(path, route.components)
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
		if len(route.components) == 0 {
			continue
		}
		parentComponents := route.components[:len(route.components)-1]
		entry := route.components[len(route.components)-1]
		parent, err := openDirectoryRoute(root, path, parentComponents, true)
		if err != nil {
			return changedCleanupError(protectUnexpected, "failed to reopen copied source parent: %v", err)
		}
		err = removeCopiedEntry(parent, directoryRoutePath(path, parentComponents), entry, protectUnexpected)
		_ = parent.Close()
		if err != nil {
			return err
		}
	}
	return nil
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

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
		return fmt.Errorf("refusing to remove directory %s because its secured name no longer identifies the opened tree", path)
	}
	if expected != nil {
		if err := validateCopiedDirectory(directory, *expected, true, path); err != nil {
			return fmt.Errorf("refusing to remove changed source tree %s: %w", path, err)
		}
	}
	if err := removeOpenedDirectoryContents(directory, path, expected); err != nil {
		return err
	}
	current, err = identityAt(parent, name)
	if err != nil || !directoryIdentity.same(current) {
		return fmt.Errorf("refusing to unlink directory %s because its secured name changed during cleanup", path)
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("failed to remove secured directory %s: %w", path, err)
	}
	return nil
}

func removeOpenedDirectoryContents(directory *os.File, path string, expected *copiedDirectory) error {
	names, err := directoryNames(directory, path)
	if err != nil {
		return err
	}
	expectedByName := make(map[string]copiedEntry)
	if expected != nil {
		for _, entry := range expected.entries {
			expectedByName[entry.name] = entry
		}
		if len(names) != len(expectedByName) {
			return fmt.Errorf("refusing to remove changed tree %s: entry set differs from copied source", path)
		}
	}
	for _, name := range names {
		entry, hasExpected := expectedByName[name]
		if expected != nil && !hasExpected {
			return fmt.Errorf("refusing to remove unexpected entry %s", filepath.Join(path, name))
		}
		current, err := identityAt(directory, name)
		if err != nil {
			return err
		}
		if hasExpected && !entry.source.same(current) {
			return fmt.Errorf("refusing to remove changed source entry %s", filepath.Join(path, name))
		}
		claimedName, err := privateMoveName("delete")
		if err != nil {
			return err
		}
		if err := renameAtNoReplace(int(directory.Fd()), name, int(directory.Fd()), claimedName); err != nil {
			return fmt.Errorf("failed to secure entry %s for removal: %w", filepath.Join(path, name), err)
		}
		claimed, err := identityAt(directory, claimedName)
		if err != nil || !current.same(claimed) {
			restoreErr := renameAtNoReplace(int(directory.Fd()), claimedName, int(directory.Fd()), name)
			return errors.Join(
				fmt.Errorf("entry %s changed while it was secured for removal", filepath.Join(path, name)),
				restoreErr,
			)
		}
		claimedPath := filepath.Join(path, claimedName)
		if current.fileType == unix.S_IFDIR {
			child, _, err := openDirectoryAt(directory, claimedName, claimedPath, "secured cleanup")
			if err != nil {
				return err
			}
			var childExpected *copiedDirectory
			if hasExpected {
				childExpected = entry.directory
			}
			err = removeOpenedDirectory(directory, claimedName, claimedPath, child, childExpected)
			_ = child.Close()
			if err != nil {
				return err
			}
			continue
		}
		if err := unix.Unlinkat(int(directory.Fd()), claimedName, 0); err != nil {
			return fmt.Errorf("failed to remove secured entry %s: %w", claimedPath, err)
		}
	}
	remaining, err := directoryNames(directory, path)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return fmt.Errorf("refusing to remove directory %s because new entries appeared during cleanup", path)
	}
	return nil
}

package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var errAtomicNoReplaceUnsupported = errors.New("atomic no-replace rename is unsupported")

func renameAtNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	err := renameAtNoReplacePlatform(oldDirFD, oldName, newDirFD, newName)
	if !isAtomicNoReplaceUnsupported(err) {
		return err
	}
	return renameAtNoReplaceCompat(oldDirFD, oldName, newDirFD, newName)
}

func isAtomicNoReplaceUnsupported(err error) bool {
	return errors.Is(err, errAtomicNoReplaceUnsupported) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EOPNOTSUPP)
}

func renamePathNoReplace(src, dest string) error {
	sourceParentPath := filepath.Dir(src)
	sourceName := filepath.Base(src)
	sourceParent, _, err := openDirectoryPathFollowingLinks(sourceParentPath, "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	sourceParentIdentity, err := identityFromFile(sourceParent)
	if err != nil {
		return err
	}
	sourceIdentity, err := identityAt(sourceParent, sourceName)
	if err != nil {
		return err
	}
	destinationParentPath := filepath.Dir(dest)
	destinationName := filepath.Base(dest)
	destinationParent, _, err := openDirectoryPathFollowingLinks(destinationParentPath, "destination parent")
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	destinationParentIdentity, err := identityFromFile(destinationParent)
	if err != nil {
		return err
	}
	if err := renameAtNoReplace(
		int(sourceParent.Fd()), sourceName,
		int(destinationParent.Fd()), destinationName,
	); err != nil {
		return err
	}
	commitErr := errors.Join(
		renamePathAfterCommit(dest),
		validateNamedPathIdentity(
			destinationParentPath, destinationName, "destination",
			destinationParentIdentity, sourceIdentity,
		),
	)
	if commitErr == nil {
		return nil
	}
	rollbackErr := rollbackRenamedPath(
		sourceParent, sourceParentPath, sourceName, sourceParentIdentity,
		destinationParent, destinationName, sourceIdentity,
	)
	return errors.Join(commitErr, rollbackErr)
}

func rollbackRenamedPath(
	sourceParent *os.File,
	sourceParentPath, sourceName string,
	sourceParentIdentity pathIdentity,
	destinationParent *os.File,
	destinationName string,
	sourceIdentity pathIdentity,
) error {
	currentDestination, err := identityAt(destinationParent, destinationName)
	if err != nil || !sourceIdentity.same(currentDestination) {
		return fmt.Errorf("could not roll back rename because the retained destination name changed")
	}
	if err := renameAtNoReplace(
		int(destinationParent.Fd()), destinationName,
		int(sourceParent.Fd()), sourceName,
	); err != nil {
		return fmt.Errorf("could not roll back rename to %s: %w", filepath.Join(sourceParentPath, sourceName), err)
	}
	if err := validateNamedPathIdentity(
		sourceParentPath, sourceName, "source", sourceParentIdentity, sourceIdentity,
	); err != nil {
		return err
	}
	return nil
}

// renameAtNoReplaceCompat uses linkat+unlinkat for non-directories. For a
// directory, which cannot be hard-linked, it reserves the absent destination
// with an empty directory and uses renameat to replace only that reservation.
// A raced-in non-empty directory cannot be replaced by a directory rename, and
// any other node type makes renameat fail, preserving no-clobber semantics.
func renameAtNoReplaceCompat(oldDirFD int, oldName string, newDirFD int, newName string) error {
	var source unix.Stat_t
	if err := unix.Fstatat(oldDirFD, oldName, &source, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if uint32(source.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return linkAtNoReplaceCompat(oldDirFD, oldName, newDirFD, newName, identityFromStat(&source))
	}
	if err := unix.Mkdirat(newDirFD, newName, 0700); err != nil {
		return err
	}
	reservationFD, err := unix.Openat(
		newDirFD,
		newName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		cleanupErr := unix.Unlinkat(newDirFD, newName, unix.AT_REMOVEDIR)
		return errors.Join(err, cleanupErr)
	}
	reservation := os.NewFile(uintptr(reservationFD), newName)
	defer reservation.Close()
	reservationIdentity, err := identityFromFile(reservation)
	if err != nil {
		cleanupErr := unix.Unlinkat(newDirFD, newName, unix.AT_REMOVEDIR)
		return errors.Join(err, cleanupErr)
	}
	if err := unix.Renameat(oldDirFD, oldName, newDirFD, newName); err != nil {
		cleanupErr := removeReservationIfUnchanged(newDirFD, newName, reservationIdentity)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func linkAtNoReplaceCompat(
	oldDirFD int,
	oldName string,
	newDirFD int,
	newName string,
	expected pathIdentity,
) error {
	if err := unix.Linkat(oldDirFD, oldName, newDirFD, newName, 0); err != nil {
		return err
	}
	if err := unix.Unlinkat(oldDirFD, oldName, 0); err != nil {
		cleanupErr := removeLinkIfUnchanged(newDirFD, newName, expected)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func removeLinkIfUnchanged(parentFD int, name string, expected pathIdentity) error {
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !expected.same(identityFromStat(&current)) {
		return fmt.Errorf("compatibility rename link %s changed; refusing rollback", name)
	}
	return unix.Unlinkat(parentFD, name, 0)
}

func removeReservationIfUnchanged(parentFD int, name string, expected pathIdentity) error {
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if !expected.same(identityFromStat(&current)) {
		return fmt.Errorf("compatibility rename reservation %s changed; refusing cleanup", name)
	}
	return unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
}

func renamePathNoReplaceCompat(src, dest string) error {
	sourceParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(src), "source parent")
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	destinationParent, _, err := openDirectoryPathFollowingLinks(filepath.Dir(dest), "destination parent")
	if err != nil {
		return err
	}
	defer destinationParent.Close()
	return renameAtNoReplaceCompat(
		int(sourceParent.Fd()), filepath.Base(src),
		int(destinationParent.Fd()), filepath.Base(dest),
	)
}

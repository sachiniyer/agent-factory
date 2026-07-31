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

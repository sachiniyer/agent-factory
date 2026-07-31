package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errAtomicNoReplaceUnsupported = errors.New("atomic no-replace rename is unsupported")

func renameAtNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return renameAtNoReplacePlatform(oldDirFD, oldName, newDirFD, newName)
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

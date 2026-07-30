//go:build darwin

package git

import "golang.org/x/sys/unix"

func renameAtNoReplace(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return unix.RenameatxNp(oldDirFD, oldName, newDirFD, newName, unix.RENAME_EXCL)
}

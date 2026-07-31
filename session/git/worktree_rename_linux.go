//go:build linux

package git

import "golang.org/x/sys/unix"

func renameAtNoReplacePlatform(oldDirFD int, oldName string, newDirFD int, newName string) error {
	return unix.Renameat2(oldDirFD, oldName, newDirFD, newName, unix.RENAME_NOREPLACE)
}

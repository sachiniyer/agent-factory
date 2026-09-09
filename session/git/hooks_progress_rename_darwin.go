//go:build darwin

package git

import "golang.org/x/sys/unix"

func renameHookProgressNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}

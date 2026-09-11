//go:build !linux

package config

import "golang.org/x/sys/unix"

func checkConfigDirectoryAccess(path string) error {
	return unix.Faccessat(unix.AT_FDCWD, path, unix.W_OK|unix.X_OK, unix.AT_EACCESS)
}

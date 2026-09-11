package config

import "golang.org/x/sys/unix"

var configDirectoryFaccessat2 = unix.Faccessat2

func checkConfigDirectoryAccess(path string) error {
	// Ask the kernel once. Do not emulate ACLs, capabilities, or classify
	// EPERM as syscall unavailability: the caller reports uncertainty.
	return configDirectoryFaccessat2(unix.AT_FDCWD, path, unix.W_OK|unix.X_OK, unix.AT_EACCESS)
}

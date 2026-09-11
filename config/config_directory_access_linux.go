package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// Kept separate from Faccessat so tests can distinguish the kernel's effective
// access check from the older-kernel fallback.
var configDirectoryFaccessat2 = unix.Faccessat2

var configDirectoryCapget = unix.Capget

func checkConfigDirectoryAccess(path string) error {
	// Call the kernel directly: unix.Faccessat can silently emulate AT_EACCESS
	// with mode bits on older Linux, losing ACL semantics.
	err := configDirectoryFaccessat2(unix.AT_FDCWD, path, unix.W_OK|unix.X_OK, unix.AT_EACCESS)
	if err != unix.ENOSYS && err != unix.EPERM {
		return err
	}

	flags := unix.AT_EACCESS
	if os.Getuid() == os.Geteuid() && os.Getgid() == os.Getegid() {
		// Non-root real-ID checks clear effective capabilities; permitted-only
		// bits are inactive for both that check and filesystem operations.
		// Root real-ID checks can substitute permitted for effective bits.
		// Search capability matters for ancestor traversal; both DAC bits
		// are in word 0.
		const dacCapabilities = 1<<unix.CAP_DAC_OVERRIDE | 1<<unix.CAP_DAC_READ_SEARCH
		hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		data := [2]unix.CapUserData{}
		if err := configDirectoryCapget(&hdr, &data[0]); err == nil {
			capabilities := data[0].Effective
			if os.Getuid() == 0 {
				capabilities |= data[0].Permitted
			}
			if capabilities&dacCapabilities == 0 {
				flags = 0
			}
		}
	}
	// For differing IDs or DAC capabilities keep x/sys's effective-access
	// fallback. Its old-kernel mode-bit emulation still cannot evaluate ACLs.
	return configDirectoryFaccessat(unix.AT_FDCWD, path, unix.W_OK|unix.X_OK, flags)
}

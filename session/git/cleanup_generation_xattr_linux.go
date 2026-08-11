//go:build linux

package git

import "golang.org/x/sys/unix"

func setCleanupGenerationCreate(fd int, name string, value []byte) error {
	return unix.Fsetxattr(fd, name, value, unix.XATTR_CREATE)
}

//go:build darwin

package git

import (
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// unix.Fsetxattr currently drops flags on Darwin. Invoke the descriptor syscall
// directly so XATTR_CREATE remains an atomic no-replace operation.
func setCleanupGenerationCreate(fd int, name string, value []byte) error {
	namePointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	var valuePointer unsafe.Pointer
	if len(value) != 0 {
		valuePointer = unsafe.Pointer(&value[0])
	}
	_, _, errno := syscall.Syscall6(
		syscall.SYS_FSETXATTR,
		uintptr(fd),
		uintptr(unsafe.Pointer(namePointer)),
		uintptr(valuePointer),
		uintptr(len(value)),
		0,
		uintptr(unix.XATTR_CREATE),
	)
	runtime.KeepAlive(value)
	if errno != 0 {
		return errno
	}
	return nil
}

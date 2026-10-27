//go:build !windows

package sessionenv

import "golang.org/x/sys/unix"

// executableAccess reports whether this process could execute path — access(2)
// rather than mode bits, mirroring the EACCES check execvp applies at each
// PATH entry before deciding a directory holds the executable.
func executableAccess(path string) bool {
	return unix.Access(path, unix.X_OK) == nil
}

//go:build !linux

package git

import (
	"os"
	"syscall"
)

// openAnonymousProbeFile — O_TMPFILE does not exist off Linux (darwin's
// tempfile story has no anonymous-inode open), so every caller there takes the
// named-probe fallback with its unlink-before-write contract.
var openAnonymousProbeFile = func(string) (*os.File, error) {
	return nil, syscall.EOPNOTSUPP
}

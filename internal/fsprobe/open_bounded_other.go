//go:build !unix

package fsprobe

import (
	"fmt"
	"os"
)

// openBoundedFile opens path for a bounded read. On platforms without
// O_NONBLOCK or O_NOFOLLOW it opens with os.Open, but it still refuses a
// non-regular file on the opened descriptor so a special file planted at the
// path does not slip into the read's flight. The FIFO-wedge hazard the unix
// guard targets is unix-only: a named pipe on these platforms is not a
// filesystem-path FIFO that blocks open, so the unguarded open is safe here.
func openBoundedFile(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	return file, nil
}

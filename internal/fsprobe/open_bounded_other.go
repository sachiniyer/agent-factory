//go:build !unix

package fsprobe

import (
	"os"
)

// OpenRegular opens path for a bounded read. On platforms without O_NONBLOCK
// or O_NOFOLLOW it opens with os.Open, but it still refuses a non-regular
// file on the opened descriptor so a special file planted at the path does
// not slip into the read's flight. The FIFO-wedge hazard the unix guard
// targets is unix-only: a named pipe on these platforms is not a
// filesystem-path FIFO that blocks open, so the unguarded open is safe here.
func OpenRegular(path string, stat RegularStatFunc) (*os.File, os.FileInfo, error) {
	return openRegular(path, nil, stat)
}

// OpenRegularPaced is OpenRegular under a paced flight's bound: step is called
// with the op name ("open" then "fstat") immediately before each syscall, so
// the flight's per-syscall bound restarts between the open and the stat rather
// than covering both under the open's window (#7527 review). stat is the
// RegularStatFunc the flight was handed when it began (#7539).
func OpenRegularPaced(path string, step func(op string), stat RegularStatFunc) (*os.File, os.FileInfo, error) {
	return openRegular(path, step, stat)
}

func openRegular(path string, step func(op string), stat RegularStatFunc) (*os.File, os.FileInfo, error) {
	if step != nil {
		step("open")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if step != nil {
		step("fstat")
	}
	info, err := RegularFileInfo(file, path, stat)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}

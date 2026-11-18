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
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := RegularFileInfo(file, path)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}

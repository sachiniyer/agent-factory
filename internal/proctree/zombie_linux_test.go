//go:build linux

package proctree

import (
	"bytes"
	"fmt"
	"os"
)

// observedZombie reports whether pid is in state Z, read straight from
// /proc/<pid>/stat rather than through any proctree helper: these tests must
// observe the zombie independently of the code under test.
//
// Format: `pid (comm) state ...`, and comm may itself contain spaces and ')',
// so the parse anchors on the LAST ')' — same reason readProc does.
func observedZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	close_ := bytes.LastIndexByte(data, ')')
	if close_ < 0 || close_+2 >= len(data) {
		return false
	}
	return data[close_+2] == 'Z'
}

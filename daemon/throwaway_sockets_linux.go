//go:build linux

package daemon

import (
	"os"
	"strconv"
	"strings"
)

// unixSocketPathsOf lists every non-empty path the kernel reports for pid's
// unix sockets — the authoritative answer to "which named sockets does this
// process hold". A client socket the kernel left unnamed never appears, so
// holding a path means the process bound or adopted that name. ok=false
// means the kernel would not show them: no socket evidence, and a tmux
// server without socket evidence is never proven private.
func unixSocketPathsOf(pid int) ([]string, bool) {
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, false
	}
	inodes := make(map[string]bool, len(ents))
	for _, e := range ents {
		tgt, err := os.Readlink(fdDir + "/" + e.Name())
		if err != nil {
			continue
		}
		ino, ok := strings.CutPrefix(tgt, "socket:[")
		if !ok {
			continue
		}
		if ino, ok := strings.CutSuffix(ino, "]"); ok {
			inodes[ino] = true
		}
	}
	if len(inodes) == 0 {
		return nil, true
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/net/unix")
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		// Num RefCount Protocol Flags Type St Inode [Path]
		if len(f) < 8 || !inodes[f[6]] {
			continue
		}
		if p := strings.Join(f[7:], " "); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

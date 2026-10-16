//go:build !windows

package sessionenv

import (
	"os"
	"syscall"
)

// spawnDirInodeID extracts the device:inode pair checkSpawnDir's FileInfo
// carries on every unix build — the spawn-tied identity the pane shim
// verifies its chdir against (#5174 review).
func spawnDirInodeID(info os.FileInfo) (dev, ino uint64, ok bool) {
	if info == nil {
		return 0, 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, false
	}
	return uint64(st.Dev), st.Ino, true
}

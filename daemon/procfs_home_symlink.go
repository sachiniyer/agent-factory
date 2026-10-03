package daemon

import (
	"os"
	"path/filepath"
)

// homeSymlinkEntersProcessRelativeProcfs reports whether home, or any symlink
// reached while resolving it, targets a process-relative procfs path that
// canonicalDir would resolve in the CALLER's frame rather than the daemon's.
// isProcessRelativeProcfsHome inspects only the home spelling, so a
// benign-looking symlink whose target is /proc/self/cwd/state (or
// /proc/thread-self/..., /dev/fd/...) bypasses the spelling guard: the daemon
// resolves the link in its own frame (<its cwd>/state) while canonicalDir
// follows the same link in the caller's frame, and a same-UID, same-namespace
// daemon launched from a different cwd compares equal to wantHome and is
// misclassified daemonOurs on a stale PID file or lone pgrep result (#4793 via
// a procfs-indirected symlink). Walk the symlink chain of the resolved home
// and any ancestor component, reading each link's immediate target, and treat
// a home whose resolved chain enters a process-relative procfs path as
// unverifiable rather than guessing ours.
//
// The chain walk reads symlink targets with os.Readlink (not EvalSymlinks,
// which would follow /proc/self in the caller's frame and lose the very
// procfs-indirection signal this guard looks for). A relative link target is
// joined against its parent directory the way the kernel resolves it.
func homeSymlinkEntersProcessRelativeProcfs(home string) bool {
	abs, err := filepath.Abs(home)
	if err != nil {
		return false
	}
	return symlinkChainEntersProcfs(filepath.Clean(abs), map[string]bool{})
}

// symlinkChainEntersProcfs is the recursive core of
// homeSymlinkEntersProcessRelativeProcfs. At each step it inspects path itself
// before descending into its parent, so a symlink at any depth of the home
// (the leaf, or an ancestor component such as /home/link/state with
// /home/link -> /proc/self/cwd) is caught. When path is a symlink it follows
// the immediate target (cleaned, and joined against the link's parent when
// relative) and recurses into it so a chain of links ending in a process-
// relative procfs path is detected. seen bounds the walk against a cycle
// (symlink loops); a non-symlink ancestor returns false at that component and
// lets the parent walk continue.
func symlinkChainEntersProcfs(path string, seen map[string]bool) bool {
	if seen[path] {
		return false
	}
	seen[path] = true
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(path); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			target = filepath.Clean(target)
			if isProcessRelativeProcfsHome(target) {
				return true
			}
			if symlinkChainEntersProcfs(target, seen) {
				return true
			}
		}
	}
	parent := filepath.Dir(path)
	if parent == path {
		return false
	}
	return symlinkChainEntersProcfs(parent, seen)
}

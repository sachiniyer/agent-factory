package daemon

import (
	"os"
	"path/filepath"
	"strings"
)

// isProcNumericMagicLink reports whether cleaned is a
// /proc/<pid>/{cwd,root,exe,fd,fdinfo,ns,map_files} magic link (with or without
// a trailing path), or the per-task form
// /proc/<pid>/task/<tid>/{cwd,root,exe,fd,fdinfo,ns,map_files}. The kernel
// resolves these per-process (and per-thread) against the process <pid> —
// /proc/<pid>/cwd is that process's live cwd, /proc/<pid>/root its root,
// /proc/<pid>/fd/<n> its open file descriptors, and /proc/<pid>/task/<tid>/cwd
// is thread <tid>'s live cwd — so a home spelled through one is the same
// cross-frame hazard isProcessRelativeProcfsHome guards for /proc/self/...: a
// same-UID, same-namespace foreign daemon launched with
// AGENT_FACTORY_HOME=/proc/<other-pid>/cwd/state (or
// /proc/<other-pid>/task/<tid>/cwd/state) serves the directory <other-pid>
// points at, while canonicalDir resolves the same spelling in the CALLER's
// frame (or the other process — or thread — chdir's after the daemon bound its
// socket), so a stale PID file or lone pgrep result can misclassify it
// daemonOurs (#4793 via a PID-addressed procfs path). A bare /proc/<pid> or a
// non-magic entry (/proc/<pid>/cmdline, /proc/<pid>/stat — regular files) is
// not a magic link.
func isProcNumericMagicLink(cleaned string) bool {
	const proc = "/proc/"
	if !strings.HasPrefix(cleaned, proc) {
		return false
	}
	rest := cleaned[len(proc):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return false // "/proc" or "/proc/<pid>" alone, not a magic link.
	}
	pidStr := rest[:slash]
	for i := 0; i < len(pidStr); i++ {
		if pidStr[i] < '0' || pidStr[i] > '9' {
			return false // /proc/self, /proc/thread-self, etc. handled by the spelling guard.
		}
	}
	entry := rest[slash+1:]
	// /proc/<pid>/task/<tid>/{cwd,root,exe,fd,fdinfo,ns,map_files} is the same
	// per-thread magic-link set the kernel resolves against <pid>/<tid>, not the
	// caller. A home spelled through one (e.g.
	// AGENT_FACTORY_HOME=/proc/<pid>/task/<tid>/cwd/state) is the same
	// cross-frame hazard as the per-process form: the first component after
	// <pid> is "task", which the magic switch below does not match, so without
	// this form the guard returned false and a /proc/<pid>/task/<tid>/... home
	// bypassed it. Recognize the task/<tid>/ form and apply the same magic-link
	// set to the per-thread entry (including its fd links); a bare
	// /proc/<pid>/task or /proc/<pid>/task/<tid> directory is not a magic link.
	if entry == "task" || strings.HasPrefix(entry, "task/") {
		tidRest := entry[len("task"):]
		if !strings.HasPrefix(tidRest, "/") {
			return false // "/proc/<pid>/task" or "/proc/<pid>/task/" alone is a directory.
		}
		tidRest = tidRest[1:]
		tidSlash := strings.IndexByte(tidRest, '/')
		if tidSlash <= 0 {
			return false // "/proc/<pid>/task/<tid>" alone is the thread directory, not a magic link.
		}
		tidStr := tidRest[:tidSlash]
		for i := 0; i < len(tidStr); i++ {
			if tidStr[i] < '0' || tidStr[i] > '9' {
				return false // a non-numeric <tid> is not a /proc task entry.
			}
		}
		entry = tidRest[tidSlash+1:]
	}
	if i := strings.IndexByte(entry, '/'); i >= 0 {
		entry = entry[:i]
	}
	switch entry {
	case "cwd", "root", "exe", "fd", "fdinfo", "ns", "map_files":
		return true
	}
	return false
}

// isProcessRelativeProcfsHome reports whether home is a /proc/self/... (or
// "/proc/self") path, or an equivalent process-relative alias the kernel
// resolves against the READING process (/proc/thread-self/..., /dev/fd/... —
// /dev/fd is a symlink to /proc/self/fd on Linux). canonicalDir resolves such a
// path in the CALLER's frame — /proc/self/cwd is the reading process's cwd,
// /proc/self/root its root — so a same-UID daemon launched from a different
// directory with AGENT_FACTORY_HOME=/proc/self/cwd/state serves <its
// cwd>/state while classifyDaemonHome resolves the same spelling against the
// CALLER's cwd, the opposite of the frame the /proc/self magic link names.
// sameProcessRoot only compares root and mount-namespace identity, not
// /proc/self resolution, so it does not catch this. classifyDaemonHome treats a
// process-relative procfs home as unverifiable rather than guessing ours and
// signalling a cross-cwd daemon (#4793 via a /proc/self magic link).
//
// A /proc/<pid>/{cwd,root,exe,fd,fdinfo,ns,map_files} magic link is the same
// hazard addressed at another process: the kernel resolves it against <pid>, not
// the caller, so a foreign same-UID, same-namespace daemon can match wantHome
// under canonicalDir where the daemon that owns the socket did not. isProcNumericMagicLink
// catches that shape too.
//
// A non-canonical spelling such as /proc//self/cwd/state (a doubled slash), or
// /proc/self/../self/cwd/state, names the same magic link after lexical
// cleaning, but the raw prefix check missed it: canonicalDir cleans the path
// and then resolves /proc/self in the caller's frame, so the raw spelling
// bypassed the guard and a foreign same-UID daemon was classified daemonOurs.
// Clean the home before the prefix test so the non-canonical form is caught the
// same way the canonical one is.
func isProcessRelativeProcfsHome(home string) bool {
	cleaned := filepath.Clean(home)
	switch {
	case cleaned == "/proc/self", strings.HasPrefix(cleaned, "/proc/self/"),
		strings.HasPrefix(cleaned, "/proc/thread-self/"),
		cleaned == "/dev/fd", strings.HasPrefix(cleaned, "/dev/fd/"):
		return true
	}
	return isProcNumericMagicLink(cleaned)
}

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
	return symlinkChainEntersProcfs(filepath.Clean(abs), "", map[string]bool{})
}

// symlinkChainEntersProcfs is the recursive core of
// homeSymlinkEntersProcessRelativeProcfs. At each step it inspects path itself
// before descending into its parent, so a symlink at any depth of the home
// (the leaf, or an ancestor component such as /home/link/state with
// /home/link -> /proc/self/cwd) is caught. When path is a symlink it follows
// the immediate target (cleaned, and joined against the link's parent when
// relative) and recurses into it so a chain of links ending in a process-
// relative procfs path is detected.
//
// suffix is the portion of the original home that sits below path (the
// components walked past toward the root): a symlink at path resolves to
// target, but the home the kernel actually opened is target joined with that
// suffix, so the rejoined path is what must be checked, not target alone. An
// ancestor alias such as /tmp/p -> /proc with
// AGENT_FACTORY_HOME=/tmp/p/self/cwd/state made the previous leaf-only walk read
// /tmp/p's target as /proc and drop the unresolved self/cwd/state, so the guard
// returned false and classifyDaemonHome canonicalized /tmp/p/self/cwd/state in
// the CALLER's frame — /proc/self/cwd is the reading process's cwd — marking a
// same-UID daemon launched from another cwd daemonOurs (#4793 via an ancestor
// procfs alias). Carrying the suffix re-joins it to /proc/self/cwd/state and
// fails closed. The suffix follows the original home's components up the parent
// walk; a symlink target's own ancestors are not walked with it (their basenames
// are not components of the home), so the suffix always names real components of
// the home rather than the target's directory layout. seen bounds the walk
// against a cycle (symlink loops) and is keyed by path plus suffix so the same
// node reached under different suffixes is still inspected; a non-symlink
// ancestor returns false at that component and lets the parent walk continue.
func symlinkChainEntersProcfs(path, suffix string, seen map[string]bool) bool {
	key := path + "\x00" + suffix
	if seen[key] {
		return false
	}
	seen[key] = true
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(path); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			target = filepath.Clean(target)
			if isProcessRelativeProcfsHome(rejoinSuffix(target, suffix)) {
				return true
			}
			if symlinkChainEntersProcfs(target, suffix, seen) {
				return true
			}
		}
	}
	parent := filepath.Dir(path)
	if parent == path {
		return false
	}
	return symlinkChainEntersProcfs(parent, filepath.Join(filepath.Base(path), suffix), seen)
}

// rejoinSuffix is the path a symlink at an ancestor of home resolves the home
// to: the ancestor's target with the suffix below it. An empty suffix (the
// home's leaf, or a chain that reached the home itself) is the target verbatim,
// so isProcessRelativeProcfsHome's existing direct-target behaviour is kept.
func rejoinSuffix(target, suffix string) string {
	if suffix == "" {
		return target
	}
	return filepath.Clean(filepath.Join(target, suffix))
}

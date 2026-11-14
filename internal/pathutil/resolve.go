package pathutil

import "path/filepath"

// ResolveForCompare resolves symlinks in a path that may not exist, for
// path-identity comparisons: the deepest existing ancestor is resolved with
// filepath.EvalSymlinks and the non-existent remainder is re-joined. Falls back
// to the cleaned input when nothing resolves, so a comparison built on it is
// never weaker than comparing Clean-ed paths.
//
// The missing-leaf case is the whole point, and getting it wrong is a
// PLATFORM-SPECIFIC bug that only shows up where the temp/working root is itself
// a symlink — macOS `/var` -> `/private/var` being the common one (#2110).
// A plain EvalSymlinks fails outright on a path whose last component was just
// deleted, so both sides of a comparison silently stay un-canonicalized and two
// spellings of the same path compare unequal. When that comparison is a probe
// ("is this still registered?"), the failure to resolve becomes a confident
// WRONG ANSWER rather than an error — so canonicalize both sides through here.
//
// The walk is unbounded: on a mount that never answers (NFS, FUSE, a dead
// sshfs) the first EvalSymlinks never returns. A caller on a poll or restore
// path uses fsprobe.ResolveForCompare, which runs this same walk inside one
// bounded flight (#7272).
func ResolveForCompare(path string) string {
	return ResolveForCompareWith(path, filepath.EvalSymlinks)
}

// ResolveForCompareWith is ResolveForCompare with eval in place of
// filepath.EvalSymlinks, for a caller that runs the walk somewhere eval can be
// substituted (fsprobe's bounded flight and its test seam). The walk and its
// missing-leaf semantics are ResolveForCompare's exactly.
func ResolveForCompareWith(path string, eval func(string) (string, error)) string {
	path = filepath.Clean(path)
	suffix := ""
	for {
		resolved, err := eval(path)
		if err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, suffix)
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = parent
	}
}

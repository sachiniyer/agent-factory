package pathutil

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

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
	resolved, _ := ResolveForCompareWith(path, filepath.EvalSymlinks)
	return resolved
}

// ResolveForCompareWith is ResolveForCompare with eval in place of
// filepath.EvalSymlinks, for a caller that runs the walk somewhere eval can be
// substituted (fsprobe's bounded flight and its test seam). The walk and its
// missing-leaf semantics are ResolveForCompare's exactly, and so is the
// returned path.
//
// err says whether that path is an answer. The walk falls back past a
// component whose lookup failed, which is right only when the failure says the
// component is absent (IsAbsent): that is the missing leaf the walk exists
// for. Any other failure (EIO, ESTALE, ENOTCONN from a soft NFS mount or a
// reconnecting sshfs, EACCES, a symlink loop) is a lookup that did not happen,
// and the fallback past it is a guess. err is the first such failure, nil when
// there was none (#7286).
func ResolveForCompareWith(path string, eval func(string) (string, error)) (resolved string, err error) {
	path = filepath.Clean(path)
	suffix := ""
	for {
		resolved, evalErr := eval(path)
		if evalErr == nil {
			return filepath.Join(resolved, suffix), err
		}
		if err == nil && !IsAbsent(evalErr) {
			err = evalErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, suffix), err
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = parent
	}
}

// IsAbsent reports whether err says a path component does not exist: ENOENT,
// or ENOTDIR for a component that is not a directory. It is the one failure
// a resolution can fall back past and still be an answer.
func IsAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

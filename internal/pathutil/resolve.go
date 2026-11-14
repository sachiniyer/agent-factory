package pathutil

import (
	"errors"
	"io/fs"
	"os"
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
	resolved, _ := ResolveForCompareWith(path, filepath.EvalSymlinks, os.Lstat)
	return resolved
}

// ErrUnresolvedComponent is ResolveForCompareWith's report of a component
// whose resolution said "absent" while the component itself is still there,
// which is what a dangling symlink does.
var ErrUnresolvedComponent = errors.New("the path component exists but does not resolve (a dangling symlink?)")

// ResolveForCompareWith is ResolveForCompare with eval and lstat in place of
// filepath.EvalSymlinks and os.Lstat, for a caller that runs the walk
// somewhere they can be substituted (fsprobe's bounded flight and its test
// seams). The walk and its missing-leaf semantics are ResolveForCompare's
// exactly, and so is the returned path.
//
// err says whether that path is an answer. The walk falls back past a
// component and rejoins it lexically, which is right only when the component is
// proven absent: its resolution failed with absence (IsAbsent), and lstat
// agrees it is not there. That is the missing leaf the walk exists for.
//   - Any other failure (EIO, ESTALE, ENOTCONN from a soft NFS mount or a
//     reconnecting sshfs, EACCES, a symlink loop) is a lookup that did not
//     happen, and the fallback past it is a guess (#7286).
//   - So is a component lstat still sees after its resolution said absent: a
//     dangling symlink, whose target the kernel's spelling went through, not
//     its lexical name (ErrUnresolvedComponent, #7295).
//
// err is the first such failure, nil when there was none. One case no walk can
// see: a symlink removed outright leaves nothing to lstat, so the lexical
// rejoin past it is reported as an answer although the kernel spelled the path
// through the link's target.
func ResolveForCompareWith(path string, eval func(string) (string, error), lstat func(string) (os.FileInfo, error)) (resolved string, err error) {
	path = filepath.Clean(path)
	suffix := ""
	for {
		resolved, evalErr := eval(path)
		if evalErr == nil {
			return filepath.Join(resolved, suffix), err
		}
		if err == nil {
			err = unprovenAbsence(path, evalErr, lstat)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, suffix), err
		}
		suffix = filepath.Join(filepath.Base(path), suffix)
		path = parent
	}
}

// unprovenAbsence is why falling back past path, whose resolution failed with
// evalErr, is a guess: evalErr itself when it is not absence, the component's
// own lstat failure when that is not absence, ErrUnresolvedComponent when lstat
// still sees it; nil when the component is proven absent.
func unprovenAbsence(path string, evalErr error, lstat func(string) (os.FileInfo, error)) error {
	if !IsAbsent(evalErr) {
		return evalErr
	}
	_, lerr := lstat(path)
	switch {
	case lerr == nil:
		return &os.PathError{Op: "resolve", Path: path, Err: ErrUnresolvedComponent}
	case !IsAbsent(lerr):
		return lerr
	}
	return nil
}

// IsAbsent reports whether err says a path component does not exist: ENOENT,
// or ENOTDIR for a component that is not a directory. It is the one failure
// a resolution can fall back past and still be an answer.
func IsAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

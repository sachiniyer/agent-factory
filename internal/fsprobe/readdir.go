package fsprobe

import (
	"io/fs"
	"os"
	"path/filepath"
)

// readDirPath and lstatPath are the test seams under ReadDir's, ReadDirInfo's
// and Lstat's bounded flights.
var (
	readDirPath      = os.ReadDir
	lstatPath        = os.Lstat
	evalSymlinksPath = filepath.EvalSymlinks
)

var (
	readDirFlights      = &flightRegistry{byDir: map[string]*probeFlight{}}
	readDirInfoFlights  = &flightRegistry{byDir: map[string]*probeFlight{}}
	lstatFlights        = &flightRegistry{byDir: map[string]*probeFlight{}}
	evalSymlinksFlights = &flightRegistry{byDir: map[string]*probeFlight{}}
)

// ReadDir lists dir inside the same per-dir bounded flight shape as Stat. A
// caller that only consults a directory it does not own, an agent's
// conversation store read on the daemon's poll and at load (#7212), cannot
// afford an unbounded listing: on a wedged mount (NFS, FUSE, a dead sshfs) the
// read never returns, and neither would the poll. A listing that does not
// answer within the bound is the ErrProbeTimedOut + context.DeadlineExceeded
// non-answer every probe here gives, and a retry while the stuck read is still
// blocked is refused the same way rather than starting another. A listing that
// answers returns what os.ReadDir does, ENOENT included.
func ReadDir(dir string) ([]os.DirEntry, error) {
	// The seam is read here, in the caller: a flight that outlives its bound
	// must never read a package variable a later caller may rewrite.
	read := readDirPath
	res, err := runBounded(dir, readDirFlights, "directory listing",
		"the filesystem did not answer the listing",
		func() (any, error) { return read(dir) })
	entries, _ := res.([]os.DirEntry)
	return entries, err
}

// ReadDirInfo is ReadDir with each entry's Info resolved inside the same
// flight: Info is an lstat per entry, which a wedged mount blocks as surely as
// the listing. The returned entries' Info answers from what the flight saw, so
// a caller can read every entry's mode and modification time without touching
// the filesystem again.
func ReadDirInfo(dir string) ([]os.DirEntry, error) {
	read := readDirPath // in the caller, as ReadDir's
	res, err := runBounded(dir, readDirInfoFlights, "directory listing",
		"the filesystem did not answer the listing",
		func() (any, error) {
			entries, err := read(dir)
			resolved := make([]os.DirEntry, len(entries))
			for idx, entry := range entries {
				info, infoErr := entry.Info()
				resolved[idx] = resolvedDirEntry{DirEntry: entry, info: info, err: infoErr}
			}
			return resolved, err
		})
	entries, _ := res.([]os.DirEntry)
	return entries, err
}

// Lstat is Stat without following a final symbolic link, in the same bounded
// flight shape: a walk rooted at path learns whether the root itself is a
// link, as filepath.WalkDir does, without an unbounded lookup.
func Lstat(path string) (os.FileInfo, error) {
	lstat := lstatPath // in the caller, as ReadDir's
	res, err := runBounded(path, lstatFlights, "lstat probe",
		"the filesystem did not answer the lstat",
		func() (any, error) { return lstat(path) })
	info, _ := res.(os.FileInfo)
	return info, err
}

// EvalSymlinks resolves path's symbolic links inside the same per-path bounded
// flight shape as ReadDir and Lstat. A caller launching a conversation that
// resolves alternate project spellings (a symlinked worktree parent) cannot
// afford an unbounded per-component lstat walk: on a wedged mount (NFS, FUSE,
// a dead sshfs) the walk never returns, and neither would the poll, the
// restore, or the status sweep that reaches it (#7212). A resolution that
// does not answer within the bound is the ErrProbeTimedOut + context.
// DeadlineExceeded non-answer every probe here gives; the caller falls back to
// the recorded spelling only, mirroring how a timed-out listing degrades to
// "could not look".
func EvalSymlinks(path string) (string, error) {
	resolve := evalSymlinksPath // in the caller, as ReadDir's
	res, err := runBounded(path, evalSymlinksFlights, "symlink resolution",
		"the filesystem did not answer the symlink resolution",
		func() (any, error) { return resolve(path) })
	resolved, _ := res.(string)
	return resolved, err
}

// resolvedDirEntry is a directory entry whose Info was read inside a flight.
type resolvedDirEntry struct {
	fs.DirEntry
	info fs.FileInfo
	err  error
}

func (e resolvedDirEntry) Info() (fs.FileInfo, error) { return e.info, e.err }

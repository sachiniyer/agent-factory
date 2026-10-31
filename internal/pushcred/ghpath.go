package pushcred

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// GHProgramName is the executable name LookGH resolves.
const GHProgramName = "gh"

// ErrGHNotFound means no absolute PATH entry holds an executable named gh.
var ErrGHNotFound = errors.New("gh is not installed in any absolute PATH directory")

// LookGH finds the gh executable on pathList the way af attributes an agent's
// executable (session.RequireAttributedAccountProgram): only ABSOLUTE PATH
// entries are searched, so an empty entry or a relative one (".", "bin", a
// repository's own tools directory) can never make the caller run a gh its
// working directory chose. Directories named in skip are passed over too, which
// is how the in-sandbox gh shim avoids resolving to itself. The result is the
// first regular file named gh that this process may execute.
//
// exec.LookPath is deliberately not used: it searches relative PATH entries
// (refusing only the implicit current directory), and the daemon's and a
// pane's PATH are both inputs af does not control.
func LookGH(pathList string, skip ...string) (string, error) {
	skipped := make(map[string]bool, len(skip))
	for _, dir := range skip {
		if dir != "" {
			skipped[filepath.Clean(dir)] = true
		}
	}
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		dir = filepath.Clean(dir)
		if skipped[dir] {
			continue
		}
		candidate := filepath.Join(dir, GHProgramName)
		if executableFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w (relative PATH entries are never searched)", ErrGHNotFound)
}

// executableFile reports whether path is a regular file this process may
// execute, judged with the effective ids as exec would.
func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}

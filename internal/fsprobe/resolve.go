package fsprobe

import (
	"fmt"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

var resolveForCompareFlights = &flightRegistry{byDir: map[string]*probeFlight{}}

// ResolveForCompare is pathutil.ResolveForCompare inside one bounded flight for
// the whole ancestor walk, not one per component (#7272). The walk resolves the
// deepest existing ancestor and re-joins the missing remainder, exactly as
// pathutil's does, through the same symlink resolution EvalSymlinks runs.
//
// A path-identity comparison on the Claude poll, restore, respawn and
// account-swap paths resolves the directory an agent launches in. A plain walk
// cannot be bounded per component: on a wedged mount (NFS, FUSE, a dead sshfs)
// the first lookup never returns, and a slow but answering one could still
// spend a bound per ancestor. A walk that does not finish within the bound is
// the ErrProbeTimedOut + context.DeadlineExceeded non-answer every probe here
// gives, and a retry while the stuck walk is still blocked is refused the same
// way. A walk that answered but fell back past a lookup that failed for any
// reason other than absence (pathutil.ResolveForCompareWith: EIO, ESTALE,
// EACCES…) is no resolution either, and returns that error (#7286). Either way
// it is "could not look", never a resolution: the caller chooses its own
// fallback, and the resolved string is empty.
func ResolveForCompare(path string) (string, error) {
	path = filepath.Clean(path)
	resolve := evalSymlinksPath.load() // in the caller, as ReadDir's
	res, err := runBounded(path, resolveForCompareFlights, "path resolution",
		"the filesystem did not answer the symlink resolution",
		func() (any, error) {
			resolved, err := pathutil.ResolveForCompareWith(path, resolve)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: a lookup failed, so the walk past it is a guess: %w", path, err)
			}
			return resolved, nil
		})
	resolved, _ := res.(string)
	return resolved, err
}

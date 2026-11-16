package session

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// The carry's plan reads both accounts' homes through its descriptor walk (an
// openat chain from an anchored root, then fstat and prefix reads), and the
// handoff engine asks that plan on its decision tick as require_carry's
// check-ahead. On a mount that stops answering, none of those syscalls
// returns, and fsprobe's path probes cannot reach them, so each read-only
// judgement the plan makes runs as a whole inside one bounded flight
// (fsprobe.Bounded, #7383). A lookup that does not answer, or fails, is a
// look that did not happen: the plan refuses the carry as unverified. It
// never carries, and it never hangs. The copy itself, which runs after the
// outgoing agent stops and must not be abandoned mid-write, is not bounded.

// key names the home r anchors, for a bounded flight's key.
func (r carryRoot) key() string {
	return r.base + "\x00" + strconv.FormatBool(r.follow) + "\x00" + strings.Join(r.components, string(filepath.Separator))
}

// carryLookupUnlooked reports whether err, from a lookup in an account's
// home, is a look that did not happen: one that did not answer within its
// bound, or a syscall that failed for a reason other than absence. A refused
// symbolic link, and a file that is not a regular file, are answers.
func carryLookupUnlooked(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, fsprobe.ErrProbeTimedOut):
		return true
	case carrySymlinkPath(err) != "", pathutil.IsAbsent(err):
		return false
	}
	var pathErr *os.PathError
	return errors.As(err, &pathErr)
}

// boundedProbeCarryDir is probeCarryDir in a bounded flight.
func boundedProbeCarryDir(home carryRoot, rel string) error {
	_, err := fsprobe.Bounded("carry store probe", home.key()+"\x00"+rel, func() (struct{}, error) {
		return struct{}{}, probeCarryDir(home, rel)
	})
	return err
}

package session

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session/git"
)

// The probes kill teardown runs at the deletion boundary itself, after every
// pane wait, for rows whose worktree cleanup must not trust the state admission
// saw before the tombstone (#3278 review, #5102).

// beforeKillTeardownOriginRecheck runs between the daemon's pre-tombstone
// admission and the teardown-boundary origin recheck below. Tests use it to
// change the origin's state inside that interval deterministically; production
// never reassigns it.
var beforeKillTeardownOriginRecheck = func() {}

// SetBeforeKillTeardownOriginRecheckForTest installs a hook in the interval
// between kill admission and the archived-origin teardown recheck, returning a
// restore function. Mirrors the git layer's restore-boundary seams.
func SetBeforeKillTeardownOriginRecheckForTest(hook func()) func() {
	previous := beforeKillTeardownOriginRecheck
	if hook == nil {
		hook = func() {}
	}
	beforeKillTeardownOriginRecheck = hook
	return func() { beforeKillTeardownOriginRecheck = previous }
}

// recheckMissingWorktreeAbsent is the deletion-boundary probe for a live row
// flagged worktree-missing (#5102), ordered like recheckOrigin's: one bounded
// lstat that does not follow links, before any cleanup command runs. Only a
// conclusive ENOENT lets the kill proceed; a probe that cannot answer and any
// occupant — directory, symlink or file alike, none of which af has verified —
// refuse as unknown, naming the path.
func recheckMissingWorktreeAbsent(path string) error {
	info, statErr := git.BoundedLstat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf(
			"%w: the session's worktree was deleted outside af, and the state of its path %s could not be established (%v) — kill the session again once the path answers",
			ErrWorkspaceStateUnknown, path, statErr,
		)
	}
	kind := "a directory"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "a symlink"
	case !info.IsDir():
		kind = "a file"
	}
	return fmt.Errorf(
		"%w: the session's worktree was deleted outside af, and %s has since appeared at %s — af has not verified it and will not delete it; inspect it, remove it or move it aside, then kill the session again",
		ErrWorkspaceStateUnknown, kind, path,
	)
}

// worktreeAbsentAtEntry reports whether a live row's recorded path
// conclusively answers ENOENT when kill teardown is prepared (#5102), one of
// the facts that arm recheckMissingWorktreeAbsent. Bounded, since it runs under
// the session operation lock; a timeout or any other error is not an absence.
func worktreeAbsentAtEntry(path string) bool {
	if path == "" {
		return false
	}
	_, err := git.BoundedLstat(path)
	return errors.Is(err, os.ErrNotExist)
}

// ArchivedCleanupSettled decides whether a settled ordinary cleanup may stand
// for an archived record-free kill (#3278 review). Nil means the pathname
// conclusively answers ENOENT — the archive is dealt with. Anything else — the
// path still occupied, or a stat that failed, stalled, or timed out rather
// than answered — returns the reason the settlement cannot be trusted, and the
// caller retains the row. Exported because the daemon's ghost cleanup shares
// the exact decision.
func ArchivedCleanupSettled(path string) error {
	if _, statErr := git.BoundedLstat(path); !errors.Is(statErr, os.ErrNotExist) {
		reason := fmt.Errorf("the archived worktree could not be proven absent at %s", path)
		if statErr != nil {
			reason = errors.Join(reason, statErr)
		}
		return reason
	}
	return nil
}

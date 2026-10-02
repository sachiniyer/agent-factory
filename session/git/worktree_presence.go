package git

import (
	"errors"
	"fmt"
	"os"
)

// WorktreePresence is the tri-state answer to "is the tracked worktree
// directory at its recorded path right now?" (#5102). It is deliberately not a
// bool: a session flagged as having lost its worktree refuses prompts and
// routes archive down a no-move path, so only a conclusive answer may produce
// Absent. Everything af cannot prove collapses to Unknown, which callers treat
// as "leave whatever was last established alone".
type WorktreePresence int

const (
	// WorktreePresenceUnknown means the question could not be answered
	// conclusively: a stat error other than ENOENT, a bounded-probe timeout, or
	// an af-owned relocation (an active claim or a durable recovery record)
	// that owns the path question. Never reported as gone.
	WorktreePresenceUnknown WorktreePresence = iota
	// WorktreePresencePresent means the recorded path exists.
	WorktreePresencePresent
	// WorktreePresenceAbsent means the recorded path is conclusively ENOENT and
	// no af relocation claims it — the worktree was deleted outside af.
	WorktreePresenceAbsent
)

// ProbeWorktreePresence answers WorktreePresence for the recorded worktree path
// with one bounded lstat. While af itself is relocating the worktree (an active
// claim) or a prior relocation left a durable recovery record, the recorded path
// can legitimately be absent mid-move; that state already has an owner which
// resolves it, so the probe defers to it with Unknown rather than flagging af's
// own move as an outside deletion.
func (g *GitWorktree) ProbeWorktreePresence() WorktreePresence {
	path, _, unresolved := g.RelocationSnapshot()
	if unresolved || path == "" {
		return WorktreePresenceUnknown
	}
	_, err := BoundedLstat(path)
	switch {
	case err == nil:
		return WorktreePresencePresent
	case errors.Is(err, os.ErrNotExist):
		return WorktreePresenceAbsent
	default:
		return WorktreePresenceUnknown
	}
}

// RepointAbsentWorktreePath rewrites the recorded worktree path to dest when the
// recorded path is conclusively absent — the restore/rename counterpart of a
// move for a worktree deleted outside af (#5102): nothing exists to relocate,
// only the record to aim at the rebuild location. It refuses while any
// relocation claim or recovery record is outstanding (that owner decides where
// the worktree is), while the recorded path still exists (that is a move, not a
// repoint), and when dest is occupied or unanswerable (the rebuild must not
// adopt or collide with a directory af did not create). Both probes run under
// relocationMu so no claim can be activated between the absence check and the
// rewrite.
func (g *GitWorktree) RepointAbsentWorktreePath(dest string) error {
	if dest == "" {
		return fmt.Errorf("cannot repoint worktree: destination path is empty")
	}
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil || g.activeRelocationClaim != nil {
		return errors.Join(fmt.Errorf(
			"cannot repoint worktree %s: a worktree relocation is still unresolved", g.worktreePath,
		), ErrRelocateStateUnknown)
	}
	current := g.worktreePath
	if current == "" {
		return fmt.Errorf("cannot repoint an empty worktree path")
	}
	if _, err := BoundedLstat(current); err == nil {
		return fmt.Errorf("cannot repoint worktree %s: it still exists; use a move", current)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot confirm worktree %s absent: %w", current, err)
	}
	if _, err := BoundedLstat(dest); err == nil {
		return fmt.Errorf("cannot repoint worktree to %s: destination occupied", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot confirm repoint destination %s free: %w", dest, err)
	}
	g.setWorktreeLocationLocked(dest)
	return nil
}

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
// with one bounded lstat. See ProbeWorktreePresenceAt.
func (g *GitWorktree) ProbeWorktreePresence() WorktreePresence {
	presence, _ := g.ProbeWorktreePresenceAt()
	return presence
}

// ProbeWorktreePresenceAt is ProbeWorktreePresence that also returns the path it
// answered for, so a caller can confirm the record still names that path when
// it acts on the answer.
//
// A durable recovery record or an activated relocation claim makes the answer
// Unknown: that state already has an owner which resolves it. This is NOT a
// complete fence against af's own moves — an ordinary archive or restore takes a
// record-free claim that is never activated, so the relocation snapshot stays
// clean for the whole move. Callers that turn Absent into state must therefore
// also check their own in-flight-operation fence and the path under the lock
// that guards that state, as Instance.RefreshWorktreeMissing does.
func (g *GitWorktree) ProbeWorktreePresenceAt() (WorktreePresence, string) {
	path, _, unresolved := g.RelocationSnapshot()
	if unresolved || path == "" {
		return WorktreePresenceUnknown, path
	}
	_, err := BoundedLstat(path)
	switch {
	case err == nil:
		return WorktreePresencePresent, path
	case errors.Is(err, os.ErrNotExist):
		// The snapshot and the lstat are two moments, and relocationMu is not held
		// across a filesystem call. A recovery-owned relocation can begin, or the
		// record be re-aimed, in between. Re-read the ownership: Absent stands only
		// if no relocation became unresolved and the record still names the path
		// that was stat'd.
		after, _, unresolvedAfter := g.RelocationSnapshot()
		if unresolvedAfter || after != path {
			return WorktreePresenceUnknown, path
		}
		return WorktreePresenceAbsent, path
	default:
		return WorktreePresenceUnknown, path
	}
}

// ErrRepointDestinationOccupied is RepointAbsentWorktreePath's refusal for a
// destination something already occupies. Restore distinguishes it because an
// occupant there may be the bytes of its own interrupted earlier attempt.
var ErrRepointDestinationOccupied = errors.New("destination occupied")

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
		return fmt.Errorf("cannot repoint worktree to %s: %w", dest, ErrRepointDestinationOccupied)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot confirm repoint destination %s free: %w", dest, err)
	}
	g.setWorktreeLocationLocked(dest)
	return nil
}

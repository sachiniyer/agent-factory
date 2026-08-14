package session

import "fmt"

// The typed recover/respawn failure shapes LocalBackend hands the daemon, split
// out of backend_local.go for the file-length lint (#1145). Both exist so the
// daemon can classify a failure without parsing error strings.

// WorktreeUnavailableError marks a recover/respawn failure caused by the
// persisted worktree path being unavailable before tmux is touched. The daemon
// uses the typed shape to add one-shot diagnostics for vanished live worktrees
// without parsing error strings (#1303).
type WorktreeUnavailableError struct {
	Title        string
	WorktreePath string
	Err          error
}

func (e *WorktreeUnavailableError) Error() string {
	return fmt.Sprintf("recover: session %q worktree unavailable: %v", e.Title, e.Err)
}

func (e *WorktreeUnavailableError) Unwrap() error {
	return e.Err
}

// RecoverRebuiltWorkspaceError marks a recovery failure that landed AFTER the
// missing worktree was rebuilt (and possibly its branch recreated): durable
// workspace state has already changed, so callers must not report an
// untouched, freely retryable failure (#3236). Error() is the inner text
// unchanged — the marker adds classification, not wording.
type RecoverRebuiltWorkspaceError struct{ Err error }

func (e *RecoverRebuiltWorkspaceError) Error() string { return e.Err.Error() }
func (e *RecoverRebuiltWorkspaceError) Unwrap() error { return e.Err }

// markRecoverRebuilt classifies a respawn failure by whether the worktree
// rebuild had already mutated durable workspace state when it happened.
func markRecoverRebuilt(rebuilt bool, err error) error {
	if !rebuilt {
		return err
	}
	return &RecoverRebuiltWorkspaceError{Err: err}
}

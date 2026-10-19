package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountTabScopeUnknownError marks a persisted account sibling whose tmux
// existence probe did not answer. The loader retains that instance inert rather
// than dropping the only handle to a process that may still be live.
type accountTabScopeUnknownError struct {
	title string
	tab   string
}

func (e *accountTabScopeUnknownError) Error() string {
	return fmt.Sprintf("restore account-scoped tab %q for %q: tmux session state is unknown", e.tab, e.title)
}

var probeRestoredTabSession = func(session *tmux.TmuxSession) (bool, bool) {
	return session.ProbeSession()
}

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

func finishRecoverTabFailure(
	title string,
	rebuilt bool,
	restoreResult tmux.RestoreResult,
	tmuxSession *tmux.TmuxSession,
	err error,
) error {
	setupErr := fmt.Errorf("recover: restore tabs for session %q: %w", title, err)
	if restoreResult == tmux.RestoreRespawned {
		if _, cleanupErr := tmuxSession.CloseAndWaitForPaneExit(); cleanupErr != nil {
			setupErr = fmt.Errorf("%w (cleanup error: %v)", setupErr, cleanupErr)
		}
	}
	return markRecoverRebuilt(rebuilt, setupErr)
}

// retainsInertInstance reports whether a startup failure is one the loader
// answers by KEEPING the row rather than dropping it — an inconclusive sibling
// probe, or a live agent whose in-place scope upgrade did not settle.
//
// It is one predicate on purpose, read by all three sites that must agree: the
// loader that decides to retain, the teardown that decides what to close, and
// the restore branch that decides whether to keep the agent handle. Those three
// used to state the rule separately and had already drifted — the handle-keeping
// site knew about one retained error and the teardown knew about none, so an
// inconclusive sibling probe closed the live agent and then retained a row whose
// every "exact cleanup handle" named a session that no longer existed.
func retainsInertInstance(err error) bool {
	var unknownScope *accountTabScopeUnknownError
	return errors.As(err, &unknownScope) || errors.Is(err, tmux.ErrAccountEnvironmentRefresh)
}

func finishLaunchTabFailure(firstTime bool, tmuxSession *tmux.TmuxSession, err error) error {
	// A retained failure is not a teardown. The agent here was REATTACHED, not
	// spawned by this load, and the inconclusive report was about a sibling —
	// closing it destroys a running agent and its scrollback to report a problem
	// with a different tab. finishRecoverTabFailure draws the same line by only
	// closing what it respawned.
	if firstTime || tmuxSession == nil || retainsInertInstance(err) {
		return err
	}
	if _, cleanupErr := tmuxSession.CloseAndWaitForPaneExit(); cleanupErr != nil {
		return fmt.Errorf("%w (cleanup error: %v)", err, cleanupErr)
	}
	return err
}

func cleanupRespawnedAccountTabs(setupErr error, sessions []*tmux.TmuxSession) error {
	if setupErr == nil {
		return nil
	}
	for _, session := range sessions {
		if _, err := session.CloseAndWaitForPaneExit(); err != nil {
			setupErr = fmt.Errorf("%w (cleanup respawned account tab: %v)", setupErr, err)
		}
	}
	return setupErr
}

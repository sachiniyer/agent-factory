package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/preflight"
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

// probeRestoredTabSession is the existence read of a restore pass. It carries
// restore-kind semantics: an unlinked socket under a possibly-live server is
// unknown, so a tab behind it is never marked inert or absent while the same
// pass's RestoreWithResult would refuse it (#6254).
var probeRestoredTabSession = func(session *tmux.TmuxSession) (bool, bool) {
	return session.ProbeSessionForRestore()
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
			setupErr = fmt.Errorf("%w (cleanup error: %w)", setupErr, cleanupErr)
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

// ErrReplacementNotDurable is a re-spawn refused because the replacement for a
// refused conversation could not be recorded durably before it started (#7209
// review R2-1). Nothing was spawned.
var ErrReplacementNotDurable = errors.New("the replacement for a refused conversation could not be recorded durably before its spawn")

// restoreDeferred reports a restore that was refused so the row can be kept
// exactly as it was and retried later: a spawn-dir refusal (#5172), a
// possibly-live server behind an unverified socket (#4135), or a replacement
// that could not be made durable before its spawn (ErrReplacementNotDurable).
// Nothing was spawned or torn down under any of them, so the persisted binding
// must survive intact, and the row loads Lost for Recover, which writes the
// replacement through its own persist hook.
func restoreDeferred(err error) bool {
	return errors.Is(err, tmux.ErrSpawnDirMissing) || errors.Is(err, tmux.ErrSessionSocketUnverified) ||
		errors.Is(err, ErrReplacementNotDurable)
}

func finishLaunchTabFailure(firstTime bool, tmuxSession *tmux.TmuxSession, err error) error {
	// A retained failure is not a teardown. The agent here was REATTACHED, not
	// spawned by this load, and the inconclusive report was about a sibling —
	// closing it destroys a running agent and its scrollback to report a problem
	// with a different tab. finishRecoverTabFailure draws the same line by only
	// closing what it respawned.
	//
	// A sibling spawn-dir refusal is the same shape: the binding (live or
	// never-spawned) is what Recover reattaches or re-spawns, and the loader
	// routes the row Lost rather than dropping it (#5172). Closing the agent
	// here would kill the live session behind the preserved handle that launch
	// just decided to keep — the contradiction retainsInertInstance's own
	// comment calls out. Mirror the two preserveAgentHandle sites in launch by
	// sparing the agent from this error too.
	//
	// An unreadable project config is never a reason to tear anything down
	// (#7077 review): it says nothing about the agent this load reattached, and
	// the row's next load reads the config again. Spare the agent for it too.
	if firstTime || tmuxSession == nil || retainsInertInstance(err) || restoreDeferred(err) || isProjectConfigUnknown(err) {
		return err
	}
	if _, cleanupErr := tmuxSession.CloseAndWaitForPaneExit(); cleanupErr != nil {
		return fmt.Errorf("%w (cleanup error: %w)", err, cleanupErr)
	}
	return err
}

func cleanupRespawnedAccountTabs(setupErr error, sessions []*tmux.TmuxSession) error {
	if setupErr == nil {
		return nil
	}
	for _, session := range sessions {
		if _, err := session.CloseAndWaitForPaneExit(); err != nil {
			setupErr = fmt.Errorf("%w (cleanup respawned account tab: %w)", setupErr, err)
		}
	}
	return setupErr
}

// worktreeStalledError is the agent-swap and account-swap paths' answer to a
// look at the worktree that did not answer within its bound (fsprobe, #7279):
// a WorktreeUnavailableError that says what to do, raised before anything is
// torn down. nil when err is not that non-answer.
func worktreeStalledError(title, workDir string, err error) error {
	if !errors.Is(err, fsprobe.ErrProbeTimedOut) {
		return nil
	}
	return &WorktreeUnavailableError{Title: title, WorktreePath: workDir, Err: fmt.Errorf(
		"the filesystem holding %s did not answer, so af left the running agent alone; "+
			"check that its mount (NFS, FUSE, sshfs) is responding, then retry the swap: %w", workDir, err)}
}

// worktreeLookupError is the swap paths' answer to a lookup of the worktree
// that did not answer or failed (#7292): the actionable
// WorktreeUnavailableError, raised before anything is torn down. A timeout
// reads as worktreeStalledError does.
func worktreeLookupError(title, workDir string, err error) error {
	if stalled := worktreeStalledError(title, workDir, err); stalled != nil {
		return stalled
	}
	return &WorktreeUnavailableError{Title: title, WorktreePath: workDir, Err: fmt.Errorf(
		"a lookup on the filesystem holding %s failed, so af left the running agent alone; "+
			"check that it is readable and its mount (NFS, FUSE, sshfs) is healthy, then retry the swap: %w", workDir, err)}
}

// swapPreflightCause is why a swap's replacement failed its launch preflight in
// workDir: the worktree that did not answer (worktreeStalledError), raised
// before anything is torn down, or preflight's own explanation of the program
// (preflight.ProgramError) for any other failure.
func swapPreflightCause(title, workDir, agent, command string, err error) error {
	if stalled := worktreeStalledError(title, workDir, err); stalled != nil {
		return stalled
	}
	return preflight.ProgramError(agent, command, err)
}
